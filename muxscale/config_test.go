package muxscale

import (
	"math"
	"testing"

	"go.viam.com/test"
)

func intp(v int) *int           { return &v }
func floatp(v float64) *float64 { return &v }
func validBase() Config         { return Config{I2CBus: "1", MuxChannel: intp(0)} }

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "minimal direct", cfg: Config{I2CBus: "1"}},
		{name: "minimal mux", cfg: validBase()},
		{name: "full", cfg: Config{
			I2CBus: "/dev/i2c-1", MuxAddress: intp(0x71), MuxChannel: intp(7), I2CAddress: intp(0x2A),
			Gain: intp(64), SampleRate: intp(320), LDOVoltage: floatp(3.0), Samples: intp(4),
			ZeroOffset: floatp(0), CalibrationFactor: floatp(-12.5), AllowNegativeWeight: true,
		}},
		{name: "missing bus", cfg: Config{MuxChannel: intp(0)}, wantErr: true},
		{name: "mux address without channel", cfg: Config{I2CBus: "1", MuxAddress: intp(0x70)}, wantErr: true},
		{name: "mux address low", cfg: Config{I2CBus: "1", MuxAddress: intp(0x6F), MuxChannel: intp(0)}, wantErr: true},
		{name: "mux address high", cfg: Config{I2CBus: "1", MuxAddress: intp(0x78), MuxChannel: intp(0)}, wantErr: true},
		{name: "channel high", cfg: Config{I2CBus: "1", MuxChannel: intp(8)}, wantErr: true},
		{name: "channel negative", cfg: Config{I2CBus: "1", MuxChannel: intp(-1)}, wantErr: true},
		{name: "i2c address reserved", cfg: Config{I2CBus: "1", I2CAddress: intp(0x03)}, wantErr: true},
		{name: "i2c address in mux range with mux", cfg: Config{I2CBus: "1", MuxChannel: intp(0), I2CAddress: intp(0x70)}, wantErr: true},
		{name: "i2c address in mux range without mux", cfg: Config{I2CBus: "1", I2CAddress: intp(0x70)}},
		{name: "bad gain", cfg: Config{I2CBus: "1", Gain: intp(3)}, wantErr: true},
		{name: "bad sample rate", cfg: Config{I2CBus: "1", SampleRate: intp(100)}, wantErr: true},
		{name: "bad ldo", cfg: Config{I2CBus: "1", LDOVoltage: floatp(5)}, wantErr: true},
		{name: "zero samples", cfg: Config{I2CBus: "1", Samples: intp(0)}, wantErr: true},
		{name: "nan zero offset", cfg: Config{I2CBus: "1", ZeroOffset: floatp(math.NaN())}, wantErr: true},
		{name: "zero factor", cfg: Config{I2CBus: "1", CalibrationFactor: floatp(0)}, wantErr: true},
		{name: "inf factor", cfg: Config{I2CBus: "1", CalibrationFactor: floatp(math.Inf(1))}, wantErr: true},
		{name: "tare and offset", cfg: Config{I2CBus: "1", TareOnStart: true, ZeroOffset: floatp(1)}, wantErr: true},
		{name: "tare alone", cfg: Config{I2CBus: "1", TareOnStart: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, optDeps, err := tc.cfg.Validate("attributes")
			test.That(t, deps, test.ShouldBeEmpty)
			test.That(t, optDeps, test.ShouldBeEmpty)
			if tc.wantErr {
				test.That(t, err, test.ShouldNotBeNil)
			} else {
				test.That(t, err, test.ShouldBeNil)
			}
		})
	}
}

func TestResolveDefaults(t *testing.T) {
	r := (&Config{I2CBus: "1"}).resolve()
	test.That(t, r.muxChannel, test.ShouldEqual, -1)
	test.That(t, r.muxAddr, test.ShouldEqual, uint16(0x70))
	test.That(t, r.addr, test.ShouldEqual, uint16(0x2A))
	test.That(t, r.gain, test.ShouldEqual, 128)
	test.That(t, r.sampleRate, test.ShouldEqual, 80)
	test.That(t, r.ldoVoltage, test.ShouldEqual, 3.3)
	test.That(t, r.samples, test.ShouldEqual, 8)
	test.That(t, r.zeroOffset, test.ShouldBeNil)
	test.That(t, r.calibrationFactor, test.ShouldBeNil)
}
