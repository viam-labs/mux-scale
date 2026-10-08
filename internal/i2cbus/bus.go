// Package i2cbus shares Linux I2C buses between the resources of this module
// and scopes transactions to one channel of a TCA9548A-style multiplexer.
//
// periph's bus serialises individual transactions but offers no way to hold
// the bus across several of them, so a Bus adds its own mutex and performs the
// mux select, the caller's transaction and the mux deselect as one critical
// section. Every critical section ends with all channels disabled, so several
// muxes on one bus can each carry a device at the same address without the
// module tracking which mux is active.
package i2cbus

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"periph.io/x/conn/v3/i2c"
	"periph.io/x/conn/v3/i2c/i2creg"
	"periph.io/x/host/v3"
)

// NoMux as a channel means the device is wired directly to the bus.
const NoMux = -1

// Bus is a shared, lockable I2C bus.
type Bus struct {
	mu   sync.Mutex
	bus  i2c.Bus
	name string
}

var (
	registryMu sync.Mutex
	// registry holds one Bus per physical bus for the life of the process.
	// Buses are never closed: resources are rebuilt on every config change,
	// and closing and reopening the device node each time gains nothing.
	registry = map[string]*Bus{}
	hostOnce sync.Once
	hostErr  error
)

// Get returns the shared Bus for an I2C bus name such as "1" or
// "/dev/i2c-1", opening it on first use. Names referring to the same bus
// number yield the same Bus.
func Get(name string) (*Bus, error) {
	key := normalizeName(name)
	registryMu.Lock()
	defer registryMu.Unlock()
	if b, ok := registry[key]; ok {
		return b, nil
	}
	hostOnce.Do(func() { _, hostErr = host.Init() })
	if hostErr != nil {
		return nil, fmt.Errorf("i2cbus: initializing periph host drivers: %w", hostErr)
	}
	bus, err := i2creg.Open(name)
	if err != nil {
		return nil, fmt.Errorf("i2cbus: opening I2C bus %q: %w", name, err)
	}
	b := NewFromBus(key, bus)
	registry[key] = b
	return b, nil
}

// NewFromBus wraps an already-open bus without registering it. It exists for
// tests and for callers that manage the bus lifetime themselves.
func NewFromBus(name string, bus i2c.Bus) *Bus {
	return &Bus{bus: bus, name: name}
}

// normalizeName reduces the spellings periph accepts for a bus to its number
// so that "1", "i2c-1", "I2C1" and "/dev/i2c-1" share one registry entry.
func normalizeName(name string) string {
	s := strings.TrimSpace(name)
	for _, prefix := range []string{"/dev/i2c-", "i2c-", "I2C", "i2c"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			s = rest
			break
		}
	}
	if n, err := strconv.Atoi(s); err == nil {
		return strconv.Itoa(n)
	}
	return strings.TrimSpace(name)
}

// Name returns the normalised bus name.
func (b *Bus) Name() string {
	return b.name
}

// WithChannel runs fn with exclusive use of the bus while only the given
// channel of the mux at muxAddr is enabled, then disables all channels again.
// With channel == NoMux the bus is simply locked around fn. A failure to
// deselect is reported alongside fn's error because it would leave the mux
// exposing its device to later transactions meant for another mux.
func (b *Bus) WithChannel(muxAddr uint16, channel int, fn func(i2c.Bus) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if channel == NoMux {
		return fn(b.bus)
	}
	if channel < 0 || channel > 7 {
		return fmt.Errorf("i2cbus: mux channel %d out of range 0-7", channel)
	}
	if err := b.setMux(muxAddr, 1<<channel); err != nil {
		return errors.Join(
			fmt.Errorf("i2cbus: selecting channel %d on mux 0x%02X: %w", channel, muxAddr, err),
			b.deselect(muxAddr),
		)
	}
	return errors.Join(fn(b.bus), b.deselect(muxAddr))
}

// VerifyMux checks that a mux answers at muxAddr and reports the requested
// channel as selected, leaving all channels disabled afterwards. With
// channel == NoMux there is nothing to verify and it returns nil.
func (b *Bus) VerifyMux(muxAddr uint16, channel int) error {
	if channel == NoMux {
		return nil
	}
	if channel < 0 || channel > 7 {
		return fmt.Errorf("i2cbus: mux channel %d out of range 0-7", channel)
	}
	return b.WithChannel(muxAddr, channel, func(bus i2c.Bus) error {
		var got [1]byte
		if err := bus.Tx(muxAddr, nil, got[:]); err != nil {
			return fmt.Errorf("i2cbus: reading mux 0x%02X control register: %w", muxAddr, err)
		}
		if want := byte(1 << channel); got[0] != want {
			return fmt.Errorf("i2cbus: mux 0x%02X reports channel mask 0x%02X after selecting channel %d (expected 0x%02X)",
				muxAddr, got[0], channel, want)
		}
		return nil
	})
}

func (b *Bus) setMux(addr uint16, mask byte) error {
	return b.bus.Tx(addr, []byte{mask}, nil)
}

func (b *Bus) deselect(addr uint16) error {
	if err := b.setMux(addr, 0); err != nil {
		return fmt.Errorf("i2cbus: deselecting all channels on mux 0x%02X: %w", addr, err)
	}
	return nil
}
