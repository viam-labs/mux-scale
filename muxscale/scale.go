package muxscale

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"periph.io/x/conn/v3/i2c"

	"github.com/viam-labs/mux-scale/internal/i2cbus"
	"github.com/viam-labs/mux-scale/nau7802"
)

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newScale,
	})
}

// getBus is replaced by tests to supply an emulated bus.
var getBus = i2cbus.Get

var errClosed = errors.New("scale is closed")

// muxedConn addresses one NAU7802 through its mux channel. Each transaction
// is a complete select/transfer/deselect critical section, so the bus is only
// held for microseconds at a time and every scale on the bus can wait for its
// own conversions concurrently.
type muxedConn struct {
	bus     *i2cbus.Bus
	muxAddr uint16
	channel int
	addr    uint16
}

func (c *muxedConn) Tx(w, r []byte) error {
	return c.bus.WithChannel(c.muxAddr, c.channel, func(b i2c.Bus) error {
		return b.Tx(c.addr, w, r)
	})
}

type scale struct {
	resource.Named
	logger logging.Logger
	cfg    resolved
	dev    *nau7802.Device

	// ioMu serialises multi-transaction device operations (averaging,
	// re-initialisation, power down) so they cannot interleave.
	ioMu sync.Mutex
	// mu guards the calibration state.
	mu                sync.Mutex
	zeroOffset        *float64
	calibrationFactor *float64
	afeCalErr         bool

	// lifetime is cancelled by Close so that an in-flight reading or
	// re-initialisation releases ioMu promptly instead of running to completion.
	lifetime context.Context
	cancel   context.CancelFunc
	closed   atomic.Bool
}

func newScale(ctx context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	// The integer casts in resolve are only safe for validated attributes.
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	r := cfg.resolve()

	bus, err := getBus(r.i2cBus)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	s := &scale{
		Named:             conf.ResourceName().AsNamed(),
		logger:            logger,
		cfg:               r,
		zeroOffset:        r.zeroOffset,
		calibrationFactor: r.calibrationFactor,
		lifetime:          lifetime,
		cancel:            cancel,
	}
	if r.muxChannel != i2cbus.NoMux {
		if err := bus.VerifyMux(r.muxAddr, r.muxChannel); err != nil {
			cancel()
			return nil, fmt.Errorf("%s: %w", s.location(), err)
		}
	}
	conn := &muxedConn{bus: bus, muxAddr: r.muxAddr, channel: r.muxChannel, addr: r.addr}
	s.dev, err = nau7802.New(conn, nau7802.Options{
		Gain:       r.gain,
		SampleRate: r.sampleRate,
		LDOVoltage: r.ldoVoltage,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	rev, err := s.dev.Revision()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("NAU7802 not responding at %s: %w", s.location(), err)
	}
	logger.Infof("NAU7802 at %s responded (revision 0x%X)", s.location(), rev)

	// rdk does not Close a resource whose constructor failed, so from here on
	// every failure powers the device down rather than leave the load cell
	// excited.
	if err := s.initDevice(ctx); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	if r.tareOnStart {
		if _, err := s.tare(ctx); err != nil {
			_ = s.Close(ctx)
			return nil, fmt.Errorf("taring on start: %w", err)
		}
	}
	return s, nil
}

// location describes where the device sits, for log and error messages.
func (s *scale) location() string {
	if s.cfg.muxChannel == i2cbus.NoMux {
		return fmt.Sprintf("address 0x%02X on bus %s", s.cfg.addr, s.cfg.i2cBus)
	}
	return fmt.Sprintf("address 0x%02X behind mux 0x%02X channel %d on bus %s",
		s.cfg.addr, s.cfg.muxAddr, s.cfg.muxChannel, s.cfg.i2cBus)
}

// initDevice runs the full power-up sequence. A failed internal calibration is
// logged rather than fatal because the device still produces usable readings
// and the failure is usually transient.
func (s *scale) initDevice(ctx context.Context) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.closed.Load() {
		return errClosed
	}
	ctx, done := s.withLifetime(ctx)
	defer done()
	err := s.dev.Init(ctx)
	calErr := errors.Is(err, nau7802.ErrCalibration)
	if err != nil && !calErr {
		return fmt.Errorf("initializing NAU7802 at %s: %w", s.location(), err)
	}
	if calErr {
		s.logger.Warnf("NAU7802 at %s: internal AFE calibration failed twice; readings may be less accurate. Send the \"reset\" command to retry.", s.location())
	}
	s.mu.Lock()
	s.afeCalErr = calErr
	s.mu.Unlock()
	return nil
}

// withLifetime derives a context that also ends when the scale is closed.
func (s *scale) withLifetime(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// average reads the configured number of samples under the I/O lock.
func (s *scale) average(ctx context.Context) (float64, error) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.closed.Load() {
		return 0, errClosed
	}
	ctx, done := s.withLifetime(ctx)
	defer done()
	raw, err := s.dev.Average(ctx, s.cfg.samples)
	if err != nil {
		return 0, fmt.Errorf("reading NAU7802 at %s: %w", s.location(), err)
	}
	return raw, nil
}

// Readings returns the averaged raw ADC value and, once a calibration factor
// is known, the derived weight in whatever unit the factor was computed with.
func (s *scale) Readings(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	raw, err := s.average(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	zero, factor := s.zeroOffset, s.calibrationFactor
	s.mu.Unlock()
	res := map[string]interface{}{
		"raw":        raw,
		"samples":    s.cfg.samples,
		"calibrated": factor != nil,
	}
	if w, ok := weight(raw, zero, factor, s.cfg.allowNegative); ok {
		res["weight"] = w
	}
	return res, nil
}

// weight converts a raw reading to a weight. It reports false when no
// calibration factor is available so callers never emit NaN or Inf.
func weight(raw float64, zero, factor *float64, allowNegative bool) (float64, bool) {
	if factor == nil || *factor == 0 {
		return 0, false
	}
	var z float64
	if zero != nil {
		z = *zero
	}
	w := (raw - z) / *factor
	if !allowNegative && w < 0 {
		w = 0
	}
	return w, true
}

// DoCommand supports:
//
//	{"tare": true}                                   measure and apply a new zero offset
//	{"calibrate": {"known_weight": 500}}             compute the factor from a known mass
//	{"get_calibration": true}                        report the current calibration
//	{"set_calibration": {"zero_offset": 0, "calibration_factor": 1}}
//	{"reset": true}                                  re-run the device power-up sequence
//
// Values applied at runtime are not persisted; copy them into the component
// attributes to keep them across restarts.
func (s *scale) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if s.closed.Load() {
		return nil, errClosed
	}
	if _, ok := cmd["tare"]; ok {
		return s.tare(ctx)
	}
	if v, ok := cmd["calibrate"]; ok {
		return s.calibrate(ctx, v)
	}
	if _, ok := cmd["get_calibration"]; ok {
		return s.calibrationResult(), nil
	}
	if v, ok := cmd["set_calibration"]; ok {
		return s.setCalibration(v)
	}
	if _, ok := cmd["reset"]; ok {
		if err := s.initDevice(ctx); err != nil {
			return nil, err
		}
		return s.calibrationResult(), nil
	}
	keys := make([]string, 0, len(cmd))
	for k := range cmd {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return nil, fmt.Errorf("unknown command %v; supported commands: tare, calibrate, get_calibration, set_calibration, reset", keys)
}

func (s *scale) tare(ctx context.Context) (map[string]interface{}, error) {
	raw, err := s.average(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.zeroOffset = &raw
	s.mu.Unlock()
	s.logger.Infof("tared: zero_offset=%v", raw)
	return s.calibrationResult(), nil
}

func (s *scale) calibrate(ctx context.Context, arg interface{}) (map[string]interface{}, error) {
	known, ok := toFloat(arg)
	if !ok {
		m, isMap := arg.(map[string]interface{})
		if isMap {
			known, ok = toFloat(m["known_weight"])
		}
	}
	if !ok {
		return nil, errors.New(`"calibrate" requires a number or {"known_weight": <number>}`)
	}
	if known == 0 || !isFinite(known) {
		return nil, errors.New(`"known_weight" must be a finite, non-zero number`)
	}
	s.mu.Lock()
	zero := s.zeroOffset
	s.mu.Unlock()
	if zero == nil {
		return nil, errors.New(`no zero offset: send {"tare": true} with the scale empty first, or set "zero_offset"`)
	}
	raw, err := s.average(ctx)
	if err != nil {
		return nil, err
	}
	factor := (raw - *zero) / known
	if factor == 0 || !isFinite(factor) {
		return nil, fmt.Errorf("reading %v did not differ from zero offset %v; is the weight on the scale?", raw, *zero)
	}
	s.mu.Lock()
	s.calibrationFactor = &factor
	s.mu.Unlock()
	s.logger.Infof("calibrated: zero_offset=%v calibration_factor=%v", *zero, factor)
	return s.calibrationResult(), nil
}

func (s *scale) setCalibration(arg interface{}) (map[string]interface{}, error) {
	m, ok := arg.(map[string]interface{})
	if !ok {
		return nil, errors.New(`"set_calibration" requires {"zero_offset": <number>, "calibration_factor": <number>}`)
	}
	var zero, factor *float64
	if v, present := m["zero_offset"]; present {
		f, ok := toFloat(v)
		if !ok || !isFinite(f) {
			return nil, errors.New(`"zero_offset" must be a finite number`)
		}
		zero = &f
	}
	if v, present := m["calibration_factor"]; present {
		f, ok := toFloat(v)
		if !ok || !isFinite(f) || f == 0 {
			return nil, errors.New(`"calibration_factor" must be a finite, non-zero number`)
		}
		factor = &f
	}
	if zero == nil && factor == nil {
		return nil, errors.New(`"set_calibration" needs "zero_offset" and/or "calibration_factor"`)
	}
	s.mu.Lock()
	if zero != nil {
		s.zeroOffset = zero
	}
	if factor != nil {
		s.calibrationFactor = factor
	}
	s.mu.Unlock()
	return s.calibrationResult(), nil
}

func (s *scale) calibrationResult() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := map[string]interface{}{
		"calibrated": s.calibrationFactor != nil,
	}
	if s.zeroOffset != nil {
		res["zero_offset"] = *s.zeroOffset
	}
	if s.calibrationFactor != nil {
		res["calibration_factor"] = *s.calibrationFactor
	}
	return res
}

// Status reports the effective configuration and calibration state.
func (s *scale) Status(context.Context) (map[string]interface{}, error) {
	res := s.calibrationResult()
	s.mu.Lock()
	res["afe_calibration_error"] = s.afeCalErr
	s.mu.Unlock()
	res["i2c_bus"] = s.cfg.i2cBus
	res["i2c_address"] = int(s.cfg.addr)
	if s.cfg.muxChannel != i2cbus.NoMux {
		res["mux_address"] = int(s.cfg.muxAddr)
		res["mux_channel"] = s.cfg.muxChannel
	}
	res["gain"] = s.cfg.gain
	res["sample_rate"] = s.cfg.sampleRate
	res["ldo_voltage"] = s.cfg.ldoVoltage
	res["samples"] = s.cfg.samples
	res["allow_negative_weight"] = s.cfg.allowNegative
	return res, nil
}

// Close powers the device down. The shared bus stays open for the other
// scales using it.
func (s *scale) Close(context.Context) error {
	if s.closed.Swap(true) {
		return nil
	}
	s.cancel()
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if err := s.dev.PowerDown(); err != nil {
		return fmt.Errorf("powering down NAU7802 at %s: %w", s.location(), err)
	}
	return nil
}

// toFloat accepts the numeric types a DoCommand payload may carry.
func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}
