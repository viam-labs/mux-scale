// Package muxscale implements a Viam sensor component for a SparkFun Qwiic
// Scale (NAU7802) load cell amplifier, optionally reached through a Qwiic
// I2C multiplexer so that several scales can share one bus.
package muxscale

import (
	"errors"
	"fmt"
	"math"

	"go.viam.com/rdk/resource"

	"github.com/viam-labs/mux-scale/internal/i2cbus"
	"github.com/viam-labs/mux-scale/nau7802"
)

// The model triplet must match meta.json.
const (
	namespace = "viam"
	family    = "mux-scale"
	modelName = "nau7802"
)

// Model identifies this sensor model.
var Model = resource.NewModel(namespace, family, modelName)

const (
	defaultMuxAddress = 0x70
	defaultGain       = 128
	defaultSampleRate = 80
	defaultLDOVoltage = 3.3
	defaultSamples    = 8
)

// Config holds the component attributes. Numeric settings are pointers so an
// explicit zero can be told apart from an omitted attribute.
type Config struct {
	// I2CBus is the periph bus name, such as "1" or "/dev/i2c-1".
	I2CBus string `json:"i2c_bus"`
	// MuxAddress is the 7-bit address of the Qwiic mux (0x70-0x77, default 0x70).
	MuxAddress *int `json:"mux_address,omitempty"`
	// MuxChannel selects the mux channel (0-7) the scale is wired to. When
	// omitted the scale is assumed to be wired directly to the bus.
	MuxChannel *int `json:"mux_channel,omitempty"`
	// I2CAddress overrides the NAU7802 address (default 0x2A).
	I2CAddress *int `json:"i2c_address,omitempty"`
	// Gain is the PGA gain: 1, 2, 4, 8, 16, 32, 64 or 128 (default 128).
	Gain *int `json:"gain,omitempty"`
	// SampleRate is the ADC rate in SPS: 10, 20, 40, 80 or 320 (default 80).
	SampleRate *int `json:"sample_rate,omitempty"`
	// LDOVoltage is the load cell excitation voltage (default 3.3).
	LDOVoltage *float64 `json:"ldo_voltage,omitempty"`
	// Samples is how many conversions are averaged per reading (default 8).
	Samples *int `json:"samples,omitempty"`
	// ZeroOffset is the raw reading of the empty scale.
	ZeroOffset *float64 `json:"zero_offset,omitempty"`
	// CalibrationFactor is raw counts per unit of weight.
	CalibrationFactor *float64 `json:"calibration_factor,omitempty"`
	// AllowNegativeWeight disables clamping of weights below zero.
	AllowNegativeWeight bool `json:"allow_negative_weight,omitempty"`
	// TareOnStart measures the zero offset when the component starts. It
	// cannot be combined with ZeroOffset.
	TareOnStart bool `json:"tare_on_start,omitempty"`
}

// resolved is a Config with defaults applied.
type resolved struct {
	i2cBus            string
	muxAddr           uint16
	muxChannel        int
	addr              uint16
	gain              int
	sampleRate        int
	ldoVoltage        float64
	samples           int
	zeroOffset        *float64
	calibrationFactor *float64
	allowNegative     bool
	tareOnStart       bool
}

func (c *Config) resolve() resolved {
	r := resolved{
		i2cBus:            c.I2CBus,
		muxAddr:           defaultMuxAddress,
		muxChannel:        i2cbus.NoMux,
		addr:              nau7802.DefaultAddress,
		gain:              defaultGain,
		sampleRate:        defaultSampleRate,
		ldoVoltage:        defaultLDOVoltage,
		samples:           defaultSamples,
		zeroOffset:        c.ZeroOffset,
		calibrationFactor: c.CalibrationFactor,
		allowNegative:     c.AllowNegativeWeight,
		tareOnStart:       c.TareOnStart,
	}
	if c.MuxAddress != nil {
		r.muxAddr = uint16(*c.MuxAddress)
	}
	if c.MuxChannel != nil {
		r.muxChannel = *c.MuxChannel
	}
	if c.I2CAddress != nil {
		r.addr = uint16(*c.I2CAddress)
	}
	if c.Gain != nil {
		r.gain = *c.Gain
	}
	if c.SampleRate != nil {
		r.sampleRate = *c.SampleRate
	}
	if c.LDOVoltage != nil {
		r.ldoVoltage = *c.LDOVoltage
	}
	if c.Samples != nil {
		r.samples = *c.Samples
	}
	return r
}

// Validate implements resource.ConfigValidator. The component has no
// dependencies on other resources.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if err := c.validate(); err != nil {
		return nil, nil, resource.NewConfigValidationError(path, err)
	}
	return nil, nil, nil
}

func (c *Config) validate() error {
	if c.I2CBus == "" {
		return errors.New(`"i2c_bus" is required`)
	}
	if c.MuxAddress != nil {
		if c.MuxChannel == nil {
			return errors.New(`"mux_address" is set but "mux_channel" is not; omit both for a scale wired directly to the bus`)
		}
		if a := *c.MuxAddress; a < 0x70 || a > 0x77 {
			return fmt.Errorf(`"mux_address" %d (0x%02X) out of range 112-119 (0x70-0x77)`, a, a)
		}
	}
	if c.MuxChannel != nil {
		if ch := *c.MuxChannel; ch < 0 || ch > 7 {
			return fmt.Errorf(`"mux_channel" %d out of range 0-7`, ch)
		}
	}
	if c.I2CAddress != nil {
		if a := *c.I2CAddress; a < 0x08 || a > 0x77 {
			return fmt.Errorf(`"i2c_address" %d out of range 8-119`, a)
		}
	}
	r := c.resolve()
	if r.muxChannel != i2cbus.NoMux && r.addr >= 0x70 && r.addr <= 0x77 {
		return fmt.Errorf(`"i2c_address" %d (0x%02X) falls in the mux address range 0x70-0x77 and would collide with the mux`, r.addr, r.addr)
	}
	if _, err := nau7802.GainCode(r.gain); err != nil {
		return fmt.Errorf(`"gain": %w`, err)
	}
	if _, err := nau7802.SampleRateCode(r.sampleRate); err != nil {
		return fmt.Errorf(`"sample_rate": %w`, err)
	}
	if _, err := nau7802.LDOCode(r.ldoVoltage); err != nil {
		return fmt.Errorf(`"ldo_voltage": %w`, err)
	}
	if r.samples < 1 {
		return fmt.Errorf(`"samples" must be at least 1, got %d`, r.samples)
	}
	if c.ZeroOffset != nil && !isFinite(*c.ZeroOffset) {
		return errors.New(`"zero_offset" must be a finite number`)
	}
	if c.CalibrationFactor != nil && (!isFinite(*c.CalibrationFactor) || *c.CalibrationFactor == 0) {
		return errors.New(`"calibration_factor" must be a finite, non-zero number`)
	}
	if c.TareOnStart && c.ZeroOffset != nil {
		return errors.New(`"tare_on_start" and "zero_offset" cannot both be set`)
	}
	return nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
