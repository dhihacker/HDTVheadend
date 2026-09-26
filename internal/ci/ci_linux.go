//go:build linux

// Package ci provides a low-level driver for a real hardware DVB CI/CI+
// Common Interface CAM slot via /dev/dvb/adapterN/ca0 — the Linux kernel's
// ca.h ioctl API (CA_RESET, CA_GET_CAP, CA_GET_SLOT_INFO, CA_GET_MSG,
// CA_SEND_MSG, CA_SET_DESCR).
//
// This talks to a physically inserted CAM using the operator's own
// licensed subscriber smart card — the same kind of module used in
// hotel/SMATV headends and set-top boxes. It is not a card-sharing client:
// there's no network protocol here, just the local hardware transport.
//
// What this package gives you: device open/reset, capability and slot
// discovery, and raw TPDU send/receive over the link layer. Building the
// full en50221 session/application layer on top (module registration,
// CA-PMT delivery to authorize a program for descrambling, MMI) is
// CAM/driver-specific integration work left for the caller — the
// en50221 spec is public but its correct implementation needs testing
// against real CAM hardware, which isn't available in this environment.
package ci

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	iocNone  = 0
	iocRead  = 2
	iocWrite = 1
)

func ioc(dir, typ, nr, size uintptr) uintptr { return dir<<30 | typ<<8 | nr | size<<16 }
func ioR(typ byte, nr, size uintptr) uintptr { return ioc(iocRead, uintptr(typ), nr, size) }
func ioW(typ byte, nr, size uintptr) uintptr { return ioc(iocWrite, uintptr(typ), nr, size) }
func io0(typ byte, nr uintptr) uintptr       { return ioc(iocNone, uintptr(typ), nr, 0) }

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Slot type bits (struct ca_slot_info.type).
const (
	CACI     = 1
	CACILink = 2
	CACIPhys = 4
	CADescr  = 8
	CASC     = 128
)

// Slot flag bits.
const (
	CAModulePresent = 1
	CAModuleReady   = 2
)

type SlotInfo struct {
	Num   int32
	Type  int32
	Flags uint32
}

type Caps struct {
	SlotNum   uint32
	SlotType  uint32
	DescrNum  uint32
	DescrType uint32
}

type msgWire struct {
	Index  uint32
	Type   uint32
	Length uint32
	Msg    [256]byte
}

type descrWire struct {
	Index  uint32
	Parity uint32
	CW     [8]byte
}

var (
	caReset       = io0('o', 128)
	caGetCap      = ioR('o', 129, unsafe.Sizeof(Caps{}))
	caGetSlotInfo = ioR('o', 130, unsafe.Sizeof(SlotInfo{}))
	caGetMsg      = ioR('o', 132, unsafe.Sizeof(msgWire{}))
	caSendMsg     = ioW('o', 133, unsafe.Sizeof(msgWire{}))
	caSetDescr    = ioW('o', 134, unsafe.Sizeof(descrWire{}))
)

// Module is an open CI/CI+ CAM slot device.
type Module struct {
	f *os.File
}

func Open(adapter, ca int) (*Module, error) {
	path := fmt.Sprintf("/dev/dvb/adapter%d/ca%d", adapter, ca)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("ci: open %s: %w", path, err)
	}
	return &Module{f: f}, nil
}

func (m *Module) Close() error { return m.f.Close() }

// Reset power-cycles the CAM slot. Call this once after opening, then poll
// SlotInfo until CAModuleReady is set before talking to the CAM.
func (m *Module) Reset() error {
	return ioctl(m.f.Fd(), caReset, nil)
}

func (m *Module) Caps() (Caps, error) {
	var c Caps
	err := ioctl(m.f.Fd(), caGetCap, unsafe.Pointer(&c))
	return c, err
}

// SlotInfo queries one CA slot's type and module-present/ready flags. num
// is the slot index (0 for the first/only slot on most hardware).
func (m *Module) SlotInfo(num int) (SlotInfo, error) {
	info := SlotInfo{Num: int32(num)}
	err := ioctl(m.f.Fd(), caGetSlotInfo, unsafe.Pointer(&info))
	return info, err
}

// SendTPDU writes a raw link-layer TPDU to the CAM.
func (m *Module) SendTPDU(data []byte) error {
	if len(data) > 256 {
		return fmt.Errorf("ci: TPDU too large (%d > 256)", len(data))
	}
	var w msgWire
	w.Length = uint32(len(data))
	copy(w.Msg[:], data)
	return ioctl(m.f.Fd(), caSendMsg, unsafe.Pointer(&w))
}

// RecvTPDU reads one pending raw link-layer TPDU from the CAM.
func (m *Module) RecvTPDU() ([]byte, error) {
	var w msgWire
	if err := ioctl(m.f.Fd(), caGetMsg, unsafe.Pointer(&w)); err != nil {
		return nil, err
	}
	n := w.Length
	if n > uint32(len(w.Msg)) {
		n = uint32(len(w.Msg))
	}
	out := make([]byte, n)
	copy(out, w.Msg[:n])
	return out, nil
}

// SetDescramblerKey loads a control word into a built-in hardware
// descrambler slot (CA type CADescr), as opposed to CAM-side descrambling.
// index selects the descrambler slot; parity is 0 (even) or 1 (odd).
func (m *Module) SetDescramblerKey(index int, parity int, cw [8]byte) error {
	w := descrWire{Index: uint32(index), Parity: uint32(parity), CW: cw}
	return ioctl(m.f.Fd(), caSetDescr, unsafe.Pointer(&w))
}
