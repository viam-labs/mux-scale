// Package nau7802test provides an in-memory NAU7802 register emulation for
// testing drivers and components without hardware.
package nau7802test

import (
	"errors"
	"fmt"
	"sync"
)

// Register and bit positions mirrored here so the fake does not depend on the
// driver's unexported constants.
const (
	regPUCtrl  = 0x00
	regCtrl2   = 0x02
	regADCOB2  = 0x12
	regMax     = 0x20
	bitRR      = 0
	bitPUD     = 1
	bitPUR     = 3
	bitCR      = 5
	bitCALS    = 2
	bitCalErr  = 3
	defaultRev = 0x0F
)

// Write records one register write made by the driver.
type Write struct {
	Reg, Val byte
}

// Fake emulates an NAU7802's register file and the handful of autonomous
// behaviours a driver depends on:
//
//   - PUR (power-up ready) asserts as soon as PUD is set.
//   - CR (conversion ready) asserts after ReadyAfterPolls reads of PU_CTRL
//     following each ADC read, and clears when the ADC output is read.
//   - CALS self-clears after CalPolls reads of CTRL2, setting CAL_ERR if
//     CalErr is true.
//   - Writing RR clears every other register.
//
// All fields may be set before use; the zero value is a healthy device that is
// always ready and calibrates successfully. Fake is safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	regs [regMax]byte
	// adc holds the 24-bit output bytes, MSB first.
	adc [3]byte

	// ReadyAfterPolls is how many PU_CTRL reads must occur after an ADC read
	// before CR asserts again. Zero means a conversion is always available.
	ReadyAfterPolls int
	// NeverReady keeps CR clear forever, to exercise timeouts.
	NeverReady bool
	// CalPolls is how many CTRL2 reads a calibration takes to complete.
	CalPolls int
	// CalErr makes every calibration finish with CAL_ERR set.
	CalErr bool
	// CalErrCount makes only the next N calibrations fail; it is decremented
	// as they complete. It is ignored when CalErr is true.
	CalErrCount int
	// TxErr, when non-nil, is returned from every transaction.
	TxErr error

	pollsSinceRead int
	calPollsLeft   int
	calibrations   int
	writes         []Write
}

// New returns a Fake reporting the SparkFun-typical revision code.
func New() *Fake {
	f := &Fake{}
	f.regs[0x1F] = defaultRev
	return f
}

// SetADC sets the conversion result the device will report.
func (f *Fake) SetADC(v int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := uint32(v)
	f.adc = [3]byte{byte(u >> 16), byte(u >> 8), byte(u)}
}

// SetADCBytes sets the raw 24-bit output bytes, MSB first.
func (f *Fake) SetADCBytes(b2, b1, b0 byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adc = [3]byte{b2, b1, b0}
}

// Reg returns the current value of a register.
func (f *Fake) Reg(reg byte) byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regs[reg]
}

// Writes returns every register write made so far, in order.
func (f *Fake) Writes() []Write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Write(nil), f.writes...)
}

// Calibrations returns how many calibrations have completed.
func (f *Fake) Calibrations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calibrations
}

// Tx implements the driver's Conn interface using the register-pointer
// convention: w[0] selects the register, further bytes of w are written to
// consecutive registers, and r is filled from consecutive registers.
func (f *Fake) Tx(w, r []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.TxErr != nil {
		return f.TxErr
	}
	if len(w) == 0 && len(r) == 0 {
		return nil
	}
	if len(w) == 0 {
		return errors.New("nau7802test: read without register pointer")
	}
	reg := w[0]
	if int(reg)+max(len(w)-1, len(r)) > regMax {
		return fmt.Errorf("nau7802test: register access at 0x%02X runs past the register map", reg)
	}
	for i, v := range w[1:] {
		f.write(reg+byte(i), v)
	}
	for i := range r {
		r[i] = f.read(reg + byte(i))
	}
	return nil
}

func (f *Fake) write(reg, val byte) {
	f.writes = append(f.writes, Write{reg, val})
	if reg == regPUCtrl {
		if val&(1<<bitRR) != 0 {
			rev := f.regs[0x1F]
			f.regs = [regMax]byte{}
			f.regs[0x1F] = rev
			f.regs[regPUCtrl] = 1 << bitRR
			return
		}
		// PUR and CR are read-only on the real device.
		val &^= 1<<bitPUR | 1<<bitCR
	}
	f.regs[reg] = val
	if reg == regCtrl2 && val&(1<<bitCALS) != 0 {
		f.calPollsLeft = f.CalPolls
	}
}

func (f *Fake) read(reg byte) byte {
	switch {
	case reg == regPUCtrl:
		v := f.regs[reg]
		if v&(1<<bitPUD) != 0 {
			v |= 1 << bitPUR
		}
		v &^= 1 << bitCR
		if !f.NeverReady {
			if f.pollsSinceRead >= f.ReadyAfterPolls {
				v |= 1 << bitCR
			}
			f.pollsSinceRead++
		}
		return v
	case reg == regCtrl2:
		v := f.regs[reg]
		if v&(1<<bitCALS) != 0 {
			if f.calPollsLeft <= 0 {
				v &^= 1 << bitCALS
				if f.CalErr || f.CalErrCount > 0 {
					v |= 1 << bitCalErr
				} else {
					v &^= 1 << bitCalErr
				}
				if !f.CalErr && f.CalErrCount > 0 {
					f.CalErrCount--
				}
				f.calibrations++
				f.regs[reg] = v
			} else {
				f.calPollsLeft--
			}
		}
		return v
	case reg >= regADCOB2 && reg < regADCOB2+3:
		f.pollsSinceRead = 0
		return f.adc[reg-regADCOB2]
	}
	return f.regs[reg]
}
