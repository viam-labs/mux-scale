// Package i2cbustest provides an emulated I2C bus carrying TCA9548A muxes and
// register devices, for testing without hardware.
package i2cbustest

import (
	"errors"
	"fmt"
	"sync"

	"periph.io/x/conn/v3/i2c"
	"periph.io/x/conn/v3/physic"
)

// Conn is a device that handles a write-then-read transaction addressed to it.
type Conn interface {
	Tx(w, r []byte) error
}

// ErrNoAck is returned when no device is reachable at an address.
var ErrNoAck = errors.New("i2cbustest: no device acknowledged")

// ErrConflict is returned when more than one device is reachable at an
// address, which on real hardware would corrupt the transaction.
var ErrConflict = errors.New("i2cbustest: multiple devices reachable at address")

type endpoint struct {
	mux     uint16
	channel int
	addr    uint16
}

// Bus is an emulated i2c.Bus. Devices behind a mux are reachable only while
// that mux has their channel enabled.
type Bus struct {
	mu     sync.Mutex
	muxes  map[uint16]byte
	direct map[uint16]Conn
	behind map[endpoint]Conn
	log    []uint16
	// Err, when non-nil, is returned by every transaction.
	Err error
}

var _ i2c.Bus = (*Bus)(nil)

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{
		muxes:  map[uint16]byte{},
		direct: map[uint16]Conn{},
		behind: map[endpoint]Conn{},
	}
}

// AddMux attaches a mux with all channels disabled.
func (b *Bus) AddMux(addr uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.muxes[addr]; !ok {
		b.muxes[addr] = 0
	}
}

// AddDirect attaches a device straight to the bus.
func (b *Bus) AddDirect(addr uint16, c Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.direct[addr] = c
}

// AddBehindMux attaches a device to one channel of a mux, adding the mux if
// needed.
func (b *Bus) AddBehindMux(muxAddr uint16, channel int, addr uint16, c Conn) {
	b.AddMux(muxAddr)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.behind[endpoint{muxAddr, channel, addr}] = c
}

// MuxMask returns the channel mask a mux currently holds.
func (b *Bus) MuxMask(addr uint16) byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.muxes[addr]
}

// Addresses returns the address of every transaction so far, in order.
func (b *Bus) Addresses() []uint16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint16(nil), b.log...)
}

// ResetLog clears the transaction log.
func (b *Bus) ResetLog() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = nil
}

// String implements i2c.Bus.
func (b *Bus) String() string { return "fake-i2c" }

// SetSpeed implements i2c.Bus.
func (b *Bus) SetSpeed(physic.Frequency) error { return nil }

// Tx implements i2c.Bus.
func (b *Bus) Tx(addr uint16, w, r []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, addr)
	if b.Err != nil {
		return b.Err
	}
	if len(w) == 0 && len(r) == 0 {
		return nil
	}
	if mask, ok := b.muxes[addr]; ok {
		if len(w) > 0 {
			mask = w[len(w)-1]
			b.muxes[addr] = mask
		}
		for i := range r {
			r[i] = mask
		}
		return nil
	}
	var targets []Conn
	if c, ok := b.direct[addr]; ok {
		targets = append(targets, c)
	}
	for ep, c := range b.behind {
		if ep.addr == addr && b.muxes[ep.mux]&(1<<ep.channel) != 0 {
			targets = append(targets, c)
		}
	}
	switch len(targets) {
	case 0:
		return fmt.Errorf("%w at 0x%02X", ErrNoAck, addr)
	case 1:
		return targets[0].Tx(w, r)
	default:
		return fmt.Errorf("%w 0x%02X", ErrConflict, addr)
	}
}
