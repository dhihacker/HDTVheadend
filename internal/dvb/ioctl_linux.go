//go:build linux

// Package dvb implements tuning and TS capture for real DVB-S/S2/T/T2/C
// tuner hardware via the Linux DVB API v5 (S2API), talking directly to
// /dev/dvb/adapterN/{frontendM,demuxM,dvrM} — no external dvb library.
//
// The ioctl request numbers below are computed the same way the kernel's
// own _IO/_IOR/_IOW macros compute them (see
// include/uapi/linux/dvb/{frontend,dmx,ca}.h), from the exact wire structs
// defined in this package. This has been checked against the current
// upstream kernel headers, but — as with any DVB driver code — needs a
// pass on real tuner hardware before being trusted in production.
package dvb

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr uintptr, size uintptr) uintptr {
	return dir<<30 | typ<<8 | nr | size<<16
}

func ioW(typ byte, nr uintptr, size uintptr) uintptr {
	return ioc(iocWrite, uintptr(typ), nr, size)
}

func ioR(typ byte, nr uintptr, size uintptr) uintptr {
	return ioc(iocRead, uintptr(typ), nr, size)
}

func io0(typ byte, nr uintptr) uintptr {
	return ioc(iocNone, uintptr(typ), nr, 0)
}

func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlPtr(f *os.File, req uintptr, ptr unsafe.Pointer) error {
	return ioctl(f.Fd(), req, uintptr(ptr))
}

func ioctlInt(f *os.File, req uintptr, val int) error {
	return ioctl(f.Fd(), req, uintptr(val))
}
