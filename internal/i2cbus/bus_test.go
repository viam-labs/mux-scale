package i2cbus

import (
	"errors"
	"sync"
	"testing"

	"go.viam.com/test"
	"periph.io/x/conn/v3/i2c"

	"github.com/viam-labs/mux-scale/internal/i2cbus/i2cbustest"
)

// echoDev records transactions and answers reads with its id.
type echoDev struct {
	id    byte
	calls int
}

func (d *echoDev) Tx(w, r []byte) error {
	d.calls++
	for i := range r {
		r[i] = d.id
	}
	return nil
}

const (
	muxAddr = 0x70
	devAddr = 0x2A
)

func readOne(addr uint16) (func(i2c.Bus) error, *byte) {
	var got byte
	return func(bus i2c.Bus) error {
		var buf [1]byte
		if err := bus.Tx(addr, []byte{0x00}, buf[:]); err != nil {
			return err
		}
		got = buf[0]
		return nil
	}, &got
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"1":          "1",
		" 1 ":        "1",
		"/dev/i2c-1": "1",
		"i2c-1":      "1",
		"I2C1":       "1",
		"i2c7":       "7",
		"01":         "1",
		"custom":     "custom",
	}
	for in, want := range cases {
		test.That(t, normalizeName(in), test.ShouldEqual, want)
	}
}

func TestWithChannelRoutesAndDeselects(t *testing.T) {
	fb := i2cbustest.NewBus()
	dev0 := &echoDev{id: 10}
	dev3 := &echoDev{id: 13}
	fb.AddBehindMux(muxAddr, 0, devAddr, dev0)
	fb.AddBehindMux(muxAddr, 3, devAddr, dev3)
	b := NewFromBus("1", fb)

	fn, got := readOne(devAddr)
	test.That(t, b.WithChannel(muxAddr, 3, fn), test.ShouldBeNil)
	test.That(t, *got, test.ShouldEqual, byte(13))
	test.That(t, fb.MuxMask(muxAddr), test.ShouldEqual, byte(0))

	fn, got = readOne(devAddr)
	test.That(t, b.WithChannel(muxAddr, 0, fn), test.ShouldBeNil)
	test.That(t, *got, test.ShouldEqual, byte(10))
	test.That(t, fb.MuxMask(muxAddr), test.ShouldEqual, byte(0))

	// With no channel selected the devices are unreachable.
	fn, _ = readOne(devAddr)
	err := b.WithChannel(muxAddr, NoMux, fn)
	test.That(t, errors.Is(err, i2cbustest.ErrNoAck), test.ShouldBeTrue)

	// A channel with nothing attached is an acknowledgement failure, and the
	// mux is still released afterwards.
	fn, _ = readOne(devAddr)
	err = b.WithChannel(muxAddr, 5, fn)
	test.That(t, errors.Is(err, i2cbustest.ErrNoAck), test.ShouldBeTrue)
	test.That(t, fb.MuxMask(muxAddr), test.ShouldEqual, byte(0))

	test.That(t, dev0.calls, test.ShouldEqual, 1)
	test.That(t, dev3.calls, test.ShouldEqual, 1)
}

func TestWithChannelNoMuxSkipsMuxTraffic(t *testing.T) {
	fb := i2cbustest.NewBus()
	fb.AddDirect(devAddr, &echoDev{id: 42})
	b := NewFromBus("1", fb)
	fn, got := readOne(devAddr)
	test.That(t, b.WithChannel(muxAddr, NoMux, fn), test.ShouldBeNil)
	test.That(t, *got, test.ShouldEqual, byte(42))
	test.That(t, fb.Addresses(), test.ShouldResemble, []uint16{devAddr})
}

func TestWithChannelRejectsBadChannel(t *testing.T) {
	b := NewFromBus("1", i2cbustest.NewBus())
	err := b.WithChannel(muxAddr, 8, func(i2c.Bus) error { return nil })
	test.That(t, err, test.ShouldNotBeNil)
	err = b.WithChannel(muxAddr, -2, func(i2c.Bus) error { return nil })
	test.That(t, err, test.ShouldNotBeNil)
}

func TestWithChannelErrorPathsReleaseLock(t *testing.T) {
	fb := i2cbustest.NewBus()
	fb.AddBehindMux(muxAddr, 1, devAddr, &echoDev{id: 1})
	b := NewFromBus("1", fb)

	fnErr := errors.New("boom")
	err := b.WithChannel(muxAddr, 1, func(i2c.Bus) error { return fnErr })
	test.That(t, errors.Is(err, fnErr), test.ShouldBeTrue)
	test.That(t, fb.MuxMask(muxAddr), test.ShouldEqual, byte(0))

	// Make the deselect fail from inside fn; both errors must surface.
	busErr := errors.New("bus died")
	err = b.WithChannel(muxAddr, 1, func(i2c.Bus) error {
		fb.Err = busErr
		return fnErr
	})
	test.That(t, errors.Is(err, fnErr), test.ShouldBeTrue)
	test.That(t, errors.Is(err, busErr), test.ShouldBeTrue)

	// Select failure is reported and does not wedge the bus.
	err = b.WithChannel(muxAddr, 1, func(i2c.Bus) error { return nil })
	test.That(t, errors.Is(err, busErr), test.ShouldBeTrue)
	fb.Err = nil

	fn, got := readOne(devAddr)
	test.That(t, b.WithChannel(muxAddr, 1, fn), test.ShouldBeNil)
	test.That(t, *got, test.ShouldEqual, byte(1))
}

func TestVerifyMux(t *testing.T) {
	fb := i2cbustest.NewBus()
	fb.AddMux(muxAddr)
	b := NewFromBus("1", fb)
	test.That(t, b.VerifyMux(muxAddr, 6), test.ShouldBeNil)
	test.That(t, fb.MuxMask(muxAddr), test.ShouldEqual, byte(0))

	err := b.VerifyMux(0x71, 6)
	test.That(t, errors.Is(err, i2cbustest.ErrNoAck), test.ShouldBeTrue)
}

func TestGetSharesBusAcrossSpellings(t *testing.T) {
	registryMu.Lock()
	saved := registry
	registry = map[string]*Bus{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})

	b := NewFromBus("1", i2cbustest.NewBus())
	registryMu.Lock()
	registry["1"] = b
	registryMu.Unlock()

	var wg sync.WaitGroup
	for _, name := range []string{"1", "/dev/i2c-1", "i2c-1", "I2C1", "1", "/dev/i2c-1"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Get(name)
			test.That(t, err, test.ShouldBeNil)
			test.That(t, got, test.ShouldEqual, b)
		}()
	}
	wg.Wait()
}

func TestVerifyMuxNoMuxAndBadChannel(t *testing.T) {
	b := NewFromBus("1", i2cbustest.NewBus())
	test.That(t, b.VerifyMux(muxAddr, NoMux), test.ShouldBeNil)
	test.That(t, b.VerifyMux(muxAddr, 8), test.ShouldNotBeNil)
	test.That(t, b.VerifyMux(muxAddr, -3), test.ShouldNotBeNil)
}
