package muxscale

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/test"

	"github.com/viam-labs/mux-scale/internal/i2cbus"
	"github.com/viam-labs/mux-scale/internal/i2cbus/i2cbustest"
	"github.com/viam-labs/mux-scale/nau7802"
	"github.com/viam-labs/mux-scale/nau7802/nau7802test"
)

const (
	testMux  = uint16(0x70)
	testAddr = uint16(nau7802.DefaultAddress)
)

// useFakeBus points the constructor at fb for the rest of the test. Every
// scale created in the test shares one i2cbus.Bus, as in production.
func useFakeBus(t *testing.T, fb *i2cbustest.Bus) {
	t.Helper()
	shared := i2cbus.NewFromBus("1", fb)
	getBus = func(string) (*i2cbus.Bus, error) { return shared, nil }
	t.Cleanup(func() { getBus = i2cbus.Get })
}

func fastConfig(channel int) *Config {
	c := &Config{I2CBus: "1", SampleRate: intp(320), Samples: intp(2)}
	if channel != i2cbus.NoMux {
		c.MuxChannel = intp(channel)
	}
	return c
}

func newTestScale(t *testing.T, cfg *Config) (sensor.Sensor, *scale) {
	t.Helper()
	conf := resource.Config{Name: "cell", API: sensor.API, Model: Model, ConvertedAttributes: cfg}
	s, err := newScale(context.Background(), nil, conf, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { test.That(t, s.Close(context.Background()), test.ShouldBeNil) })
	return s, s.(*scale)
}

func readings(t *testing.T, s sensor.Sensor) map[string]interface{} {
	t.Helper()
	r, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	return r
}

func TestReadingsThroughMux(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev0, dev3 := nau7802test.New(), nau7802test.New()
	dev0.SetADC(1000)
	dev3.SetADC(-3000)
	fb.AddBehindMux(testMux, 0, testAddr, dev0)
	fb.AddBehindMux(testMux, 3, testAddr, dev3)
	useFakeBus(t, fb)

	s0, _ := newTestScale(t, fastConfig(0))
	s3, _ := newTestScale(t, fastConfig(3))

	r0 := readings(t, s0)
	test.That(t, r0["raw"], test.ShouldEqual, 1000.0)
	test.That(t, r0["calibrated"], test.ShouldBeFalse)
	test.That(t, r0["samples"], test.ShouldEqual, 2)
	_, hasWeight := r0["weight"]
	test.That(t, hasWeight, test.ShouldBeFalse)

	r3 := readings(t, s3)
	test.That(t, r3["raw"], test.ShouldEqual, -3000.0)
	test.That(t, fb.MuxMask(testMux), test.ShouldEqual, byte(0))
}

func TestReadingsWithoutMux(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	dev.SetADC(77)
	fb.AddDirect(testAddr, dev)
	useFakeBus(t, fb)

	s, _ := newTestScale(t, fastConfig(i2cbus.NoMux))
	test.That(t, readings(t, s)["raw"], test.ShouldEqual, 77.0)
	for _, addr := range fb.Addresses() {
		test.That(t, addr, test.ShouldEqual, testAddr)
	}
}

func TestConstructorFailures(t *testing.T) {
	fb := i2cbustest.NewBus()
	fb.AddBehindMux(testMux, 0, testAddr, nau7802test.New())
	useFakeBus(t, fb)
	logger := logging.NewTestLogger(t)

	// Mux present, nothing on the requested channel.
	conf := resource.Config{Name: "cell", API: sensor.API, Model: Model, ConvertedAttributes: fastConfig(5)}
	_, err := newScale(context.Background(), nil, conf, logger)
	test.That(t, errors.Is(err, i2cbustest.ErrNoAck), test.ShouldBeTrue)
	test.That(t, fb.MuxMask(testMux), test.ShouldEqual, byte(0))

	// Wrong mux address.
	cfg := fastConfig(0)
	cfg.MuxAddress = intp(0x72)
	conf.ConvertedAttributes = cfg
	_, err = newScale(context.Background(), nil, conf, logger)
	test.That(t, err, test.ShouldNotBeNil)

	// Device on the bus fails every transaction.
	broken := nau7802test.New()
	broken.TxErr = errors.New("nack")
	fb.AddBehindMux(testMux, 1, testAddr, broken)
	conf.ConvertedAttributes = fastConfig(1)
	_, err = newScale(context.Background(), nil, conf, logger)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestWeight(t *testing.T) {
	zero, factor := 1000.0, 10.0
	cases := []struct {
		raw      float64
		zero     *float64
		factor   *float64
		allowNeg bool
		want     float64
		ok       bool
	}{
		{raw: 1500, zero: &zero, factor: &factor, want: 50, ok: true},
		{raw: 500, zero: &zero, factor: &factor, want: 0, ok: true},
		{raw: 500, zero: &zero, factor: &factor, allowNeg: true, want: -50, ok: true},
		{raw: 1500, zero: nil, factor: &factor, want: 150, ok: true},
		{raw: 1500, zero: &zero, factor: nil, ok: false},
	}
	for _, c := range cases {
		got, ok := weight(c.raw, c.zero, c.factor, c.allowNeg)
		test.That(t, ok, test.ShouldEqual, c.ok)
		if ok {
			test.That(t, got, test.ShouldEqual, c.want)
		}
	}
}

func TestReadingsCalibratedFromConfig(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	dev.SetADC(1500)
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)

	cfg := fastConfig(0)
	cfg.ZeroOffset = floatp(1000)
	cfg.CalibrationFactor = floatp(10)
	s, _ := newTestScale(t, cfg)
	r := readings(t, s)
	test.That(t, r["calibrated"], test.ShouldBeTrue)
	test.That(t, r["weight"], test.ShouldEqual, 50.0)

	dev.SetADC(500)
	test.That(t, readings(t, s)["weight"], test.ShouldEqual, 0.0)

	cfg.AllowNegativeWeight = true
	s2, _ := newTestScale(t, cfg)
	test.That(t, readings(t, s2)["weight"], test.ShouldEqual, -50.0)
}

func TestDoCommandTareAndCalibrate(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	dev.SetADC(1000)
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	s, _ := newTestScale(t, fastConfig(0))
	ctx := context.Background()

	_, err := s.DoCommand(ctx, map[string]interface{}{"calibrate": 500.0})
	test.That(t, err, test.ShouldNotBeNil)

	res, err := s.DoCommand(ctx, map[string]interface{}{"tare": true})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["zero_offset"], test.ShouldEqual, 1000.0)
	test.That(t, res["calibrated"], test.ShouldBeFalse)

	dev.SetADC(6000)
	_, err = s.DoCommand(ctx, map[string]interface{}{"calibrate": map[string]interface{}{"known_weight": 0.0}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = s.DoCommand(ctx, map[string]interface{}{"calibrate": "500"})
	test.That(t, err, test.ShouldNotBeNil)

	res, err = s.DoCommand(ctx, map[string]interface{}{"calibrate": map[string]interface{}{"known_weight": 500.0}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["calibration_factor"], test.ShouldEqual, 10.0)
	test.That(t, res["calibrated"], test.ShouldBeTrue)
	test.That(t, readings(t, s)["weight"], test.ShouldEqual, 500.0)

	// Weight on the scale identical to the tare reading is rejected.
	dev.SetADC(1000)
	_, err = s.DoCommand(ctx, map[string]interface{}{"calibrate": 500.0})
	test.That(t, err, test.ShouldNotBeNil)

	res, err = s.DoCommand(ctx, map[string]interface{}{"get_calibration": true})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["zero_offset"], test.ShouldEqual, 1000.0)
	test.That(t, res["calibration_factor"], test.ShouldEqual, 10.0)

	res, err = s.DoCommand(ctx, map[string]interface{}{"set_calibration": map[string]interface{}{"zero_offset": 0.0, "calibration_factor": 2.0}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["zero_offset"], test.ShouldEqual, 0.0)
	test.That(t, res["calibration_factor"], test.ShouldEqual, 2.0)
	test.That(t, readings(t, s)["weight"], test.ShouldEqual, 500.0)

	_, err = s.DoCommand(ctx, map[string]interface{}{"set_calibration": map[string]interface{}{"calibration_factor": 0.0}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = s.DoCommand(ctx, map[string]interface{}{"set_calibration": map[string]interface{}{}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = s.DoCommand(ctx, map[string]interface{}{"frobnicate": true})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestDoCommandReset(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	s, _ := newTestScale(t, fastConfig(0))
	test.That(t, dev.Calibrations(), test.ShouldEqual, 1)
	_, err := s.DoCommand(context.Background(), map[string]interface{}{"reset": true})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, dev.Calibrations(), test.ShouldEqual, 2)
}

func TestAFECalibrationErrorIsNotFatal(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	dev.CalErr = true
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	s, _ := newTestScale(t, fastConfig(0))
	st, err := s.Status(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, st["afe_calibration_error"], test.ShouldBeTrue)
	test.That(t, st["mux_channel"], test.ShouldEqual, 0)
	readings(t, s)
}

func TestTareOnStart(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	dev.SetADC(4321)
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	cfg := fastConfig(0)
	cfg.TareOnStart = true
	s, _ := newTestScale(t, cfg)
	res, err := s.DoCommand(context.Background(), map[string]interface{}{"get_calibration": true})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["zero_offset"], test.ShouldEqual, 4321.0)
}

func TestClose(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	conf := resource.Config{Name: "cell", API: sensor.API, Model: Model, ConvertedAttributes: fastConfig(0)}
	s, err := newScale(context.Background(), nil, conf, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldBeNil)

	test.That(t, s.Close(context.Background()), test.ShouldBeNil)
	test.That(t, s.Close(context.Background()), test.ShouldBeNil)
	test.That(t, dev.Reg(nau7802.RegPUCtrl)&0b110, test.ShouldEqual, 0)

	_, err = s.Readings(context.Background(), nil)
	test.That(t, errors.Is(err, errClosed), test.ShouldBeTrue)
	_, err = s.DoCommand(context.Background(), map[string]interface{}{"tare": true})
	test.That(t, errors.Is(err, errClosed), test.ShouldBeTrue)
}

func TestConcurrentReadingsAndCommands(t *testing.T) {
	fb := i2cbustest.NewBus()
	devs := []*nau7802test.Fake{nau7802test.New(), nau7802test.New(), nau7802test.New()}
	for i, d := range devs {
		d.SetADC(int32(1000 * (i + 1)))
		fb.AddBehindMux(testMux, i, testAddr, d)
	}
	useFakeBus(t, fb)
	var scales []sensor.Sensor
	for i := range devs {
		s, _ := newTestScale(t, fastConfig(i))
		scales = append(scales, s)
	}

	var wg sync.WaitGroup
	for i, s := range scales {
		for j := 0; j < 10; j++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				r := readings(t, s)
				test.That(t, r["raw"], test.ShouldEqual, float64(1000*(i+1)))
			}()
			go func() {
				defer wg.Done()
				_, err := s.DoCommand(context.Background(), map[string]interface{}{"tare": true})
				test.That(t, err, test.ShouldBeNil)
			}()
		}
	}
	wg.Wait()
	test.That(t, fb.MuxMask(testMux), test.ShouldEqual, byte(0))
}

func TestCloseInterruptsSlowReading(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	cfg := fastConfig(0)
	cfg.SampleRate = intp(10)
	conf := resource.Config{Name: "cell", API: sensor.API, Model: Model, ConvertedAttributes: cfg}
	s, err := newScale(context.Background(), nil, conf, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldBeNil)

	// At 10 SPS a reading that never becomes ready would take a full second
	// to time out; Close must not wait for that.
	dev.NeverReady = true
	readErr := make(chan error, 1)
	go func() {
		_, err := s.Readings(context.Background(), nil)
		readErr <- err
	}()
	// Let the reading start polling before closing.
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	test.That(t, s.Close(context.Background()), test.ShouldBeNil)
	test.That(t, time.Since(start), test.ShouldBeLessThan, 500*time.Millisecond)
	err = <-readErr
	test.That(t, err, test.ShouldNotBeNil)

	_, err = s.DoCommand(context.Background(), map[string]interface{}{"reset": true})
	test.That(t, errors.Is(err, errClosed), test.ShouldBeTrue)
	test.That(t, dev.Reg(nau7802.RegPUCtrl)&0b110, test.ShouldEqual, 0)
}

func TestConstructorFailureAfterPowerUpPowersDown(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev := nau7802test.New()
	// Power-up succeeds but no conversion ever completes, so Init fails
	// while discarding the first samples.
	dev.NeverReady = true
	fb.AddBehindMux(testMux, 0, testAddr, dev)
	useFakeBus(t, fb)
	conf := resource.Config{Name: "cell", API: sensor.API, Model: Model, ConvertedAttributes: fastConfig(0)}
	_, err := newScale(context.Background(), nil, conf, logging.NewTestLogger(t))
	test.That(t, errors.Is(err, nau7802.ErrTimeout), test.ShouldBeTrue)
	test.That(t, dev.Reg(nau7802.RegPUCtrl)&0b110, test.ShouldEqual, 0)
}

func TestConstructorRejectsInvalidConfig(t *testing.T) {
	useFakeBus(t, i2cbustest.NewBus())
	cfg := &Config{I2CBus: "1", MuxChannel: intp(0), I2CAddress: intp(0x70)}
	conf := resource.Config{Name: "cell", API: sensor.API, Model: Model, ConvertedAttributes: cfg}
	_, err := newScale(context.Background(), nil, conf, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldNotBeNil)
}
