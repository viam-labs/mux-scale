package nau7802

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.viam.com/test"

	"github.com/viam-labs/mux-scale/nau7802/nau7802test"
)

func defaultOptions() Options {
	return Options{Gain: 128, SampleRate: 320, LDOVoltage: 3.3}
}

func TestGainCode(t *testing.T) {
	cases := map[int]byte{1: 0, 2: 1, 4: 2, 8: 3, 16: 4, 32: 5, 64: 6, 128: 7}
	for gain, want := range cases {
		got, err := GainCode(gain)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, got, test.ShouldEqual, want)
	}
	for _, bad := range []int{0, 3, 256, -1} {
		_, err := GainCode(bad)
		test.That(t, err, test.ShouldNotBeNil)
	}
}

func TestSampleRateCode(t *testing.T) {
	cases := map[int]byte{10: 0b000, 20: 0b001, 40: 0b010, 80: 0b011, 320: 0b111}
	for sps, want := range cases {
		got, err := SampleRateCode(sps)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, got, test.ShouldEqual, want)
	}
	for _, bad := range []int{0, 160, 640} {
		_, err := SampleRateCode(bad)
		test.That(t, err, test.ShouldNotBeNil)
	}
}

func TestLDOCode(t *testing.T) {
	cases := map[float64]byte{4.5: 0, 4.2: 1, 3.9: 2, 3.6: 3, 3.3: 4, 3.0: 5, 2.7: 6, 2.4: 7}
	for volts, want := range cases {
		got, err := LDOCode(volts)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, got, test.ShouldEqual, want)
	}
	for _, bad := range []float64{0, 3.1, 5.0} {
		_, err := LDOCode(bad)
		test.That(t, err, test.ShouldNotBeNil)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	f := nau7802test.New()
	_, err := New(f, Options{Gain: 3, SampleRate: 80, LDOVoltage: 3.3})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = New(f, Options{Gain: 128, SampleRate: 50, LDOVoltage: 3.3})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = New(f, Options{Gain: 128, SampleRate: 80, LDOVoltage: 5})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestReadRawSignExtension(t *testing.T) {
	cases := []struct {
		b2, b1, b0 byte
		want       int32
	}{
		{0x00, 0x00, 0x00, 0},
		{0x7F, 0xFF, 0xFF, 8388607},
		{0x80, 0x00, 0x00, -8388608},
		{0xFF, 0xFF, 0xFF, -1},
		{0x12, 0x34, 0x56, 1193046},
		{0xED, 0xCB, 0xAA, -1193046},
	}
	f := nau7802test.New()
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	for _, c := range cases {
		f.SetADCBytes(c.b2, c.b1, c.b0)
		got, err := d.ReadRaw()
		test.That(t, err, test.ShouldBeNil)
		test.That(t, got, test.ShouldEqual, c.want)
	}
}

func TestInitRegisterState(t *testing.T) {
	f := nau7802test.New()
	f.SetADC(1000)
	d, err := New(f, Options{Gain: 64, SampleRate: 80, LDOVoltage: 3.0})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, d.Init(context.Background()), test.ShouldBeNil)

	puCtrl := f.Reg(RegPUCtrl)
	test.That(t, puCtrl&(1<<puCtrlRR), test.ShouldEqual, 0)
	test.That(t, puCtrl&(1<<puCtrlPUD), test.ShouldNotEqual, 0)
	test.That(t, puCtrl&(1<<puCtrlPUA), test.ShouldNotEqual, 0)
	test.That(t, puCtrl&(1<<puCtrlCS), test.ShouldNotEqual, 0)
	test.That(t, puCtrl&(1<<puCtrlAVDDS), test.ShouldNotEqual, 0)

	ctrl1 := f.Reg(RegCtrl1)
	test.That(t, ctrl1&ctrl1GainsMask, test.ShouldEqual, 0b110)
	test.That(t, (ctrl1&ctrl1VLDOMask)>>ctrl1VLDOShift, test.ShouldEqual, 0b101)

	ctrl2 := f.Reg(RegCtrl2)
	test.That(t, (ctrl2&ctrl2CRSMask)>>ctrl2CRSShift, test.ShouldEqual, 0b011)
	test.That(t, ctrl2&ctrl2CalModMask, test.ShouldEqual, 0)
	test.That(t, ctrl2&(1<<ctrl2CALS), test.ShouldEqual, 0)

	test.That(t, f.Reg(RegADC)&adcClkChpOff, test.ShouldEqual, adcClkChpOff)
	test.That(t, f.Reg(RegPGAPwr)&(1<<pgaPwrCapEn), test.ShouldNotEqual, 0)
	test.That(t, f.Reg(RegPGA)&(1<<pgaLDOMode), test.ShouldEqual, 0)
	test.That(t, f.Calibrations(), test.ShouldEqual, 1)

	// The reset must happen before anything else is configured.
	writes := f.Writes()
	test.That(t, writes[0].Reg, test.ShouldEqual, RegPUCtrl)
	test.That(t, writes[0].Val&(1<<puCtrlRR), test.ShouldNotEqual, 0)
}

func TestInitRetriesCalibrationOnce(t *testing.T) {
	f := nau7802test.New()
	f.CalErrCount = 1
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, d.Init(context.Background()), test.ShouldBeNil)
	test.That(t, f.Calibrations(), test.ShouldEqual, 2)

	f = nau7802test.New()
	f.CalErr = true
	d, err = New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	err = d.Init(context.Background())
	test.That(t, errors.Is(err, ErrCalibration), test.ShouldBeTrue)
	test.That(t, f.Calibrations(), test.ShouldEqual, 2)
	// Everything before calibration still took effect.
	test.That(t, f.Reg(RegPUCtrl)&(1<<puCtrlCS), test.ShouldNotEqual, 0)
}

func TestCalibrateAFE(t *testing.T) {
	f := nau7802test.New()
	f.CalPolls = 3
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, d.CalibrateAFE(context.Background()), test.ShouldBeNil)
	test.That(t, f.Calibrations(), test.ShouldEqual, 1)

	f.CalErr = true
	err = d.CalibrateAFE(context.Background())
	test.That(t, errors.Is(err, ErrCalibration), test.ShouldBeTrue)

	f.CalErr = false
	f.CalPolls = 1 << 30
	err = d.CalibrateAFE(context.Background())
	test.That(t, errors.Is(err, ErrTimeout), test.ShouldBeTrue)
}

func TestReadSampleWaitsForReady(t *testing.T) {
	f := nau7802test.New()
	f.ReadyAfterPolls = 3
	f.SetADC(-4242)
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	v, err := d.ReadSample(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, v, test.ShouldEqual, -4242)
}

func TestReadSampleTimeout(t *testing.T) {
	f := nau7802test.New()
	f.NeverReady = true
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	_, err = d.ReadSample(context.Background())
	test.That(t, errors.Is(err, ErrTimeout), test.ShouldBeTrue)
}

func TestReadSampleHonoursContext(t *testing.T) {
	f := nau7802test.New()
	f.NeverReady = true
	d, err := New(f, Options{Gain: 128, SampleRate: 10, LDOVoltage: 3.3})
	test.That(t, err, test.ShouldBeNil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = d.ReadSample(ctx)
	test.That(t, errors.Is(err, context.Canceled), test.ShouldBeTrue)
	test.That(t, time.Since(start), test.ShouldBeLessThan, 100*time.Millisecond)
}

func TestAverage(t *testing.T) {
	f := nau7802test.New()
	f.SetADC(100)
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	avg, err := d.Average(context.Background(), 4)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, avg, test.ShouldEqual, 100.0)
	_, err = d.Average(context.Background(), 0)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestTxErrorsPropagate(t *testing.T) {
	f := nau7802test.New()
	f.TxErr = errors.New("nack")
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	_, err = d.Revision()
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, d.Init(context.Background()), test.ShouldNotBeNil)
	test.That(t, d.PowerDown(), test.ShouldNotBeNil)
}

func TestRevisionAndPowerDown(t *testing.T) {
	f := nau7802test.New()
	d, err := New(f, defaultOptions())
	test.That(t, err, test.ShouldBeNil)
	rev, err := d.Revision()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, rev, test.ShouldEqual, 0x0F)

	test.That(t, d.Init(context.Background()), test.ShouldBeNil)
	test.That(t, d.PowerDown(), test.ShouldBeNil)
	puCtrl := f.Reg(RegPUCtrl)
	test.That(t, puCtrl&(1<<puCtrlPUD), test.ShouldEqual, 0)
	test.That(t, puCtrl&(1<<puCtrlPUA), test.ShouldEqual, 0)
	ready, err := d.getBit(RegPUCtrl, puCtrlPUR)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, ready, test.ShouldBeFalse)
}
