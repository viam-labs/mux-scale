// Package nau7802 drives the Nuvoton NAU7802 24-bit load cell ADC over I2C.
//
// The package is transport-agnostic: callers supply a Conn that performs a
// combined write-then-read I2C transaction against the device address, so the
// same driver works directly on a bus or behind an I2C multiplexer.
package nau7802

import (
	"fmt"
	"math"
)

// DefaultAddress is the fixed 7-bit I2C address of the NAU7802. The chip has
// no address pins, so multiple devices on one bus require a multiplexer.
const DefaultAddress = 0x2A

// Register addresses (datasheet section 9).
const (
	RegPUCtrl    = 0x00
	RegCtrl1     = 0x01
	RegCtrl2     = 0x02
	RegOCal1B2   = 0x03
	RegGCal1B3   = 0x06
	RegI2CCtrl   = 0x11
	RegADCOB2    = 0x12 // first of three ADC output bytes, MSB first
	RegADC       = 0x15
	RegOTPB1     = 0x16
	RegPGA       = 0x1B
	RegPGAPwr    = 0x1C
	RegDeviceRev = 0x1F
)

// PU_CTRL bit positions.
const (
	puCtrlRR    = 0 // register reset
	puCtrlPUD   = 1 // power up digital
	puCtrlPUA   = 2 // power up analog
	puCtrlPUR   = 3 // power up ready (read only)
	puCtrlCS    = 4 // cycle start
	puCtrlCR    = 5 // cycle ready: a conversion result is available (read only)
	puCtrlAVDDS = 7 // AVDD source select: 1 = internal LDO
)

// CTRL1 fields.
const (
	ctrl1GainsMask = 0b0000_0111
	ctrl1VLDOShift = 3
	ctrl1VLDOMask  = 0b0011_1000
)

// CTRL2 fields.
const (
	ctrl2CalModMask = 0b0000_0011
	ctrl2CALS       = 2 // start calibration; self-clears when done
	ctrl2CalErr     = 3
	ctrl2CRSShift   = 4
	ctrl2CRSMask    = 0b0111_0000
)

const (
	// adcClkChpOff sets ADC register bits [5:4], which disables the chopper
	// clock as recommended by datasheet section 9.1.
	adcClkChpOff = 0x30
	// pgaLDOMode selects an improved-stability/lower-accuracy LDO mode; it is
	// cleared to favour accuracy.
	pgaLDOMode = 6
	// pgaPwrCapEn enables the 330pF decoupling capacitor on channel 2
	// (datasheet section 9.14); required on the SparkFun Qwiic Scale board.
	pgaPwrCapEn = 7
)

// GainCode converts a PGA gain (1, 2, 4, ..., 128) to its CTRL1 GAINS code.
func GainCode(gain int) (byte, error) {
	switch gain {
	case 1:
		return 0b000, nil
	case 2:
		return 0b001, nil
	case 4:
		return 0b010, nil
	case 8:
		return 0b011, nil
	case 16:
		return 0b100, nil
	case 32:
		return 0b101, nil
	case 64:
		return 0b110, nil
	case 128:
		return 0b111, nil
	}
	return 0, fmt.Errorf("nau7802: unsupported gain %d (want 1, 2, 4, 8, 16, 32, 64 or 128)", gain)
}

// SampleRateCode converts a conversion rate in samples per second to its
// CTRL2 CRS code.
func SampleRateCode(sps int) (byte, error) {
	switch sps {
	case 10:
		return 0b000, nil
	case 20:
		return 0b001, nil
	case 40:
		return 0b010, nil
	case 80:
		return 0b011, nil
	case 320:
		return 0b111, nil
	}
	return 0, fmt.Errorf("nau7802: unsupported sample rate %d SPS (want 10, 20, 40, 80 or 320)", sps)
}

// ldoVoltages lists the selectable LDO output voltages indexed by VLDO code.
var ldoVoltages = [...]float64{4.5, 4.2, 3.9, 3.6, 3.3, 3.0, 2.7, 2.4}

// LDOCode converts an LDO output voltage to its CTRL1 VLDO code.
func LDOCode(volts float64) (byte, error) {
	for code, v := range ldoVoltages {
		if math.Abs(v-volts) < 0.01 {
			return byte(code), nil
		}
	}
	return 0, fmt.Errorf("nau7802: unsupported LDO voltage %.2f (want one of %v)", volts, ldoVoltages)
}
