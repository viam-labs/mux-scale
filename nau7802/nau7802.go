package nau7802

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Conn performs a single I2C transaction with the NAU7802: w is written, then
// r is filled, with a repeated start in between. Either slice may be empty.
// Implementations must be safe to call from multiple goroutines.
type Conn interface {
	Tx(w, r []byte) error
}

// Options configures the analog front end.
type Options struct {
	// Gain is the PGA gain: 1, 2, 4, 8, 16, 32, 64 or 128.
	Gain int
	// SampleRate is the conversion rate in SPS: 10, 20, 40, 80 or 320.
	SampleRate int
	// LDOVoltage is the internal LDO output used to excite the load cell:
	// 2.4, 2.7, 3.0, 3.3, 3.6, 3.9, 4.2 or 4.5.
	LDOVoltage float64
}

// ErrCalibration is returned when the device's internal analog front end
// calibration completes with its error flag set. When Init returns an error
// that wraps ErrCalibration the device is otherwise fully configured and
// readable.
var ErrCalibration = errors.New("nau7802: AFE calibration reported an error")

// ErrTimeout is returned when the device does not produce a conversion or
// finish calibrating within the expected time.
var ErrTimeout = errors.New("nau7802: timed out waiting for device")

const (
	powerUpPollInterval = time.Millisecond
	powerUpPollAttempts = 100
	ldoSettleDelay      = 250 * time.Millisecond
	calibrationTimeout  = time.Second
	calibrationPoll     = time.Millisecond
	// discardSamples is the number of conversions dropped after Init, since
	// the first results after power-up are unreliable.
	discardSamples = 10
)

// Device is a driver for one NAU7802.
type Device struct {
	c        Conn
	gainCode byte
	rateCode byte
	ldoCode  byte
	// period is the nominal time between conversions at the configured rate.
	period time.Duration
}

// New validates opts and returns a Device bound to c. No I/O is performed.
func New(c Conn, opts Options) (*Device, error) {
	gain, err := GainCode(opts.Gain)
	if err != nil {
		return nil, err
	}
	rate, err := SampleRateCode(opts.SampleRate)
	if err != nil {
		return nil, err
	}
	ldo, err := LDOCode(opts.LDOVoltage)
	if err != nil {
		return nil, err
	}
	return &Device{
		c:        c,
		gainCode: gain,
		rateCode: rate,
		ldoCode:  ldo,
		period:   time.Second / time.Duration(opts.SampleRate),
	}, nil
}

// Init resets and powers up the device, applies the configured LDO voltage,
// gain and sample rate, discards the first conversions, and runs the internal
// AFE calibration. The sequence follows the SparkFun Qwiic Scale library.
//
// A returned error wrapping ErrCalibration means every step except the final
// calibration succeeded; the caller may choose to keep using the device.
func (d *Device) Init(ctx context.Context) error {
	if err := d.reset(ctx); err != nil {
		return err
	}
	if err := d.powerUp(ctx); err != nil {
		return err
	}
	if err := d.rmw(RegCtrl1, ctrl1VLDOMask, d.ldoCode<<ctrl1VLDOShift); err != nil {
		return fmt.Errorf("nau7802: setting LDO voltage: %w", err)
	}
	if err := d.setBit(RegPUCtrl, puCtrlAVDDS); err != nil {
		return fmt.Errorf("nau7802: selecting internal LDO: %w", err)
	}
	if err := d.rmw(RegCtrl1, ctrl1GainsMask, d.gainCode); err != nil {
		return fmt.Errorf("nau7802: setting gain: %w", err)
	}
	if err := d.rmw(RegCtrl2, ctrl2CRSMask, d.rateCode<<ctrl2CRSShift); err != nil {
		return fmt.Errorf("nau7802: setting sample rate: %w", err)
	}
	if err := d.rmw(RegADC, adcClkChpOff, adcClkChpOff); err != nil {
		return fmt.Errorf("nau7802: disabling chopper clock: %w", err)
	}
	if err := d.setBit(RegPGAPwr, pgaPwrCapEn); err != nil {
		return fmt.Errorf("nau7802: enabling PGA capacitor: %w", err)
	}
	if err := d.clearBit(RegPGA, pgaLDOMode); err != nil {
		return fmt.Errorf("nau7802: clearing LDO mode: %w", err)
	}
	if err := sleepCtx(ctx, ldoSettleDelay); err != nil {
		return err
	}
	for i := 0; i < discardSamples; i++ {
		if _, err := d.ReadSample(ctx); err != nil {
			return fmt.Errorf("nau7802: discarding initial samples: %w", err)
		}
	}
	// Calibration errors are frequently transient, so one retry is worthwhile.
	err := d.CalibrateAFE(ctx)
	if errors.Is(err, ErrCalibration) {
		err = d.CalibrateAFE(ctx)
	}
	return err
}

func (d *Device) reset(ctx context.Context) error {
	if err := d.setBit(RegPUCtrl, puCtrlRR); err != nil {
		return fmt.Errorf("nau7802: asserting reset: %w", err)
	}
	if err := sleepCtx(ctx, time.Millisecond); err != nil {
		return err
	}
	if err := d.clearBit(RegPUCtrl, puCtrlRR); err != nil {
		return fmt.Errorf("nau7802: releasing reset: %w", err)
	}
	return nil
}

func (d *Device) powerUp(ctx context.Context) error {
	if err := d.setBit(RegPUCtrl, puCtrlPUD); err != nil {
		return fmt.Errorf("nau7802: powering up digital: %w", err)
	}
	if err := d.setBit(RegPUCtrl, puCtrlPUA); err != nil {
		return fmt.Errorf("nau7802: powering up analog: %w", err)
	}
	ready := false
	for i := 0; i < powerUpPollAttempts && !ready; i++ {
		var err error
		ready, err = d.getBit(RegPUCtrl, puCtrlPUR)
		if err != nil {
			return fmt.Errorf("nau7802: polling power-up ready: %w", err)
		}
		if !ready {
			if err := sleepCtx(ctx, powerUpPollInterval); err != nil {
				return err
			}
		}
	}
	if !ready {
		return fmt.Errorf("%w: power-up ready never asserted", ErrTimeout)
	}
	if err := d.setBit(RegPUCtrl, puCtrlCS); err != nil {
		return fmt.Errorf("nau7802: starting conversions: %w", err)
	}
	return nil
}

// Revision returns the 4-bit device revision code. It is also the cheapest
// way to confirm a device acknowledges on the bus.
func (d *Device) Revision() (byte, error) {
	rev, err := d.readReg(RegDeviceRev)
	if err != nil {
		return 0, fmt.Errorf("nau7802: reading revision: %w", err)
	}
	return rev & 0x0F, nil
}

// Available reports whether a new conversion result is waiting to be read.
func (d *Device) Available() (bool, error) {
	return d.getBit(RegPUCtrl, puCtrlCR)
}

// ReadRaw reads the latest 24-bit two's-complement conversion result without
// checking whether it is new.
func (d *Device) ReadRaw() (int32, error) {
	var buf [3]byte
	if err := d.c.Tx([]byte{RegADCOB2}, buf[:]); err != nil {
		return 0, fmt.Errorf("nau7802: reading ADC: %w", err)
	}
	// Place the 24-bit value in the top of an int32 so the arithmetic shift
	// sign-extends it.
	v := int32(uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8)
	return v >> 8, nil
}

// ReadSample waits for the next conversion to complete and returns it. It
// polls at a fraction of the conversion period and gives up after several
// periods, returning ErrTimeout.
func (d *Device) ReadSample(ctx context.Context) (int32, error) {
	interval := max(d.period/4, time.Millisecond)
	timeout := max(10*d.period, 100*time.Millisecond)
	deadline := time.Now().Add(timeout)
	for {
		ready, err := d.Available()
		if err != nil {
			return 0, err
		}
		if ready {
			return d.ReadRaw()
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("%w: no conversion within %v", ErrTimeout, timeout)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return 0, err
		}
	}
}

// Average returns the mean of n consecutive conversions.
func (d *Device) Average(ctx context.Context, n int) (float64, error) {
	if n < 1 {
		return 0, fmt.Errorf("nau7802: sample count must be at least 1, got %d", n)
	}
	var sum int64
	for i := 0; i < n; i++ {
		v, err := d.ReadSample(ctx)
		if err != nil {
			return 0, err
		}
		sum += int64(v)
	}
	return float64(sum) / float64(n), nil
}

// CalibrateAFE runs the device's internal offset and gain calibration and
// waits for it to finish. It returns ErrCalibration if the device flags an
// error and ErrTimeout if calibration does not complete.
func (d *Device) CalibrateAFE(ctx context.Context) error {
	if err := d.rmw(RegCtrl2, ctrl2CalModMask, 0); err != nil {
		return fmt.Errorf("nau7802: selecting internal calibration: %w", err)
	}
	if err := d.setBit(RegCtrl2, ctrl2CALS); err != nil {
		return fmt.Errorf("nau7802: starting calibration: %w", err)
	}
	deadline := time.Now().Add(calibrationTimeout)
	for {
		ctrl2, err := d.readReg(RegCtrl2)
		if err != nil {
			return fmt.Errorf("nau7802: polling calibration: %w", err)
		}
		if ctrl2&(1<<ctrl2CALS) == 0 {
			if ctrl2&(1<<ctrl2CalErr) != 0 {
				return ErrCalibration
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: calibration did not finish within %v", ErrTimeout, calibrationTimeout)
		}
		if err := sleepCtx(ctx, calibrationPoll); err != nil {
			return err
		}
	}
}

// PowerDown switches off the analog and digital sections. Init brings the
// device back.
func (d *Device) PowerDown() error {
	if err := d.clearBit(RegPUCtrl, puCtrlPUD); err != nil {
		return fmt.Errorf("nau7802: powering down digital: %w", err)
	}
	if err := d.clearBit(RegPUCtrl, puCtrlPUA); err != nil {
		return fmt.Errorf("nau7802: powering down analog: %w", err)
	}
	return nil
}

func (d *Device) readReg(reg byte) (byte, error) {
	var buf [1]byte
	if err := d.c.Tx([]byte{reg}, buf[:]); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func (d *Device) writeReg(reg, val byte) error {
	return d.c.Tx([]byte{reg, val}, nil)
}

// rmw replaces the bits selected by mask with val, leaving the rest intact.
func (d *Device) rmw(reg, mask, val byte) error {
	cur, err := d.readReg(reg)
	if err != nil {
		return err
	}
	return d.writeReg(reg, (cur&^mask)|(val&mask))
}

func (d *Device) setBit(reg, bit byte) error {
	return d.rmw(reg, 1<<bit, 1<<bit)
}

func (d *Device) clearBit(reg, bit byte) error {
	return d.rmw(reg, 1<<bit, 0)
}

func (d *Device) getBit(reg, bit byte) (bool, error) {
	v, err := d.readReg(reg)
	if err != nil {
		return false, err
	}
	return v&(1<<bit) != 0, nil
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
