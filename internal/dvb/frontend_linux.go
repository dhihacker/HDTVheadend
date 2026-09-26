//go:build linux

package dvb

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"
)

// Delivery systems (enum fe_delivery_system).
const (
	sysUndefined  = 0
	sysDVBCAnnexA = 1
	sysDVBT       = 3
	sysDVBS       = 5
	sysDVBS2      = 6
	sysDVBT2      = 16
	sysDVBCAnnexC = 18
)

// Modulation (enum fe_modulation).
const (
	modQPSK    = 0
	modQAM16   = 1
	modQAM32   = 2
	modQAM64   = 3
	modQAM128  = 4
	modQAM256  = 5
	modQAMAuto = 6
	modPSK8    = 9
)

// Inversion / FEC "auto" values.
const (
	inversionAuto = 2
	fecAuto       = 9
)

// DTV property command IDs (linux/dvb/frontend.h).
const (
	dtvTune           = 1
	dtvClear          = 2
	dtvFrequency      = 3
	dtvModulation     = 4
	dtvBandwidthHz    = 5
	dtvInversion      = 6
	dtvSymbolRate     = 8
	dtvInnerFEC       = 9
	dtvDeliverySystem = 17
)

// fe_status bits.
const (
	feHasLock = 0x10
)

// fe_sec_voltage / fe_sec_tone_mode.
const (
	secVoltage13 = 0
	secVoltage18 = 1
	secToneOff   = 1
)

// dtvProperty mirrors struct dtv_property (linux/dvb/frontend.h), which is
// __attribute__((packed)) in the kernel header, sized 76 bytes there (4 +
// 12 + 56-byte union + 4). Go's default field layout matches that size and
// those offsets here because every field boundary already falls on a
// naturally aligned offset for this shape — but changing field order or
// types would break that equivalence.
type dtvProperty struct {
	Cmd      uint32
	Reserved [3]uint32
	U        [56]byte // union { u32 data; struct dtv_fe_stats st; buffer struct }; we only ever use the leading u32.
	Result   int32
}

func newDTVProperty(cmd uint32, data uint32) dtvProperty {
	var p dtvProperty
	p.Cmd = cmd
	binary.LittleEndian.PutUint32(p.U[0:4], data)
	return p
}

// dtvProperties mirrors struct dtv_properties: { __u32 num; struct
// dtv_property *props; }. Go inserts the same platform-specific padding
// before the pointer field that a C compiler would.
type dtvProperties struct {
	Num   uint32
	Props uintptr
}

// dvbFrontendInfo mirrors struct dvb_frontend_info.
type dvbFrontendInfo struct {
	Name                [128]byte
	Type                int32
	FrequencyMin        uint32
	FrequencyMax        uint32
	FrequencyStepsize   uint32
	FrequencyTolerance  uint32
	SymbolRateMin       uint32
	SymbolRateMax       uint32
	SymbolRateTolerance uint32
	NotifierDelay       uint32
	Caps                int32
}

var (
	feGetInfo     = ioR('o', 61, unsafe.Sizeof(dvbFrontendInfo{}))
	feReadStatus  = ioR('o', 69, 4) // fe_status_t (enum, 4 bytes)
	feSetVoltage  = io0('o', 67)
	feSetTone     = io0('o', 66)
	feSetProperty = ioW('o', 82, unsafe.Sizeof(dtvProperties{}))
)

// Frontend controls one DVB tuner (/dev/dvb/adapterN/frontendM).
type Frontend struct {
	f *os.File
}

func OpenFrontend(adapter, frontend int) (*Frontend, error) {
	path := fmt.Sprintf("/dev/dvb/adapter%d/frontend%d", adapter, frontend)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("dvb: open %s: %w", path, err)
	}
	return &Frontend{f: f}, nil
}

func (fe *Frontend) Close() error { return fe.f.Close() }

// Info reads the tuner's driver-reported name.
func (fe *Frontend) Info() (string, error) {
	var info dvbFrontendInfo
	if err := ioctlPtr(fe.f, feGetInfo, unsafe.Pointer(&info)); err != nil {
		return "", fmt.Errorf("dvb: get info: %w", err)
	}
	n := 0
	for n < len(info.Name) && info.Name[n] != 0 {
		n++
	}
	return string(info.Name[:n]), nil
}

// TuneParams are the parameters needed to lock onto a transponder/mux.
type TuneParams struct {
	System       string // "DVBS", "DVBS2", "DVBT", "DVBT2", "DVBC"
	FrequencyKHz uint32 // DVB-S/S2: L-band IF frequency in kHz. DVB-T/T2/C: RF frequency, converted to Hz internally.
	SymbolRateKS uint32 // DVB-S/S2/C, in kilosymbols/s
	Polarization string // "H", "V", "L", "R" — DVB-S/S2, drives 13V/18V LNB voltage
	BandwidthHz  uint32 // DVB-T/T2/C
	Modulation   string // "QPSK", "8PSK", "QAM64", "QAM256", "AUTO", ...
}

func delsysOf(s string) (uint32, error) {
	switch s {
	case "DVBS":
		return sysDVBS, nil
	case "DVBS2":
		return sysDVBS2, nil
	case "DVBT":
		return sysDVBT, nil
	case "DVBT2":
		return sysDVBT2, nil
	case "DVBC":
		return sysDVBCAnnexA, nil
	default:
		return 0, fmt.Errorf("dvb: unknown delivery system %q", s)
	}
}

func modulationOf(s string) uint32 {
	switch s {
	case "QPSK":
		return modQPSK
	case "8PSK", "PSK8":
		return modPSK8
	case "QAM16":
		return modQAM16
	case "QAM32":
		return modQAM32
	case "QAM64":
		return modQAM64
	case "QAM128":
		return modQAM128
	case "QAM256":
		return modQAM256
	default:
		return modQAMAuto
	}
}

// Tune sets voltage/tone for satellite polarization as needed, then applies
// the S2API property set and waits up to timeout for a lock.
func (fe *Frontend) Tune(p TuneParams, timeout time.Duration) error {
	delsys, err := delsysOf(p.System)
	if err != nil {
		return err
	}

	isSat := p.System == "DVBS" || p.System == "DVBS2"
	if isSat {
		voltage := secVoltage13
		if p.Polarization == "V" || p.Polarization == "R" {
			voltage = secVoltage18
		}
		if err := ioctlInt(fe.f, feSetVoltage, voltage); err != nil {
			return fmt.Errorf("dvb: set voltage: %w", err)
		}
		if err := ioctlInt(fe.f, feSetTone, secToneOff); err != nil {
			return fmt.Errorf("dvb: set tone: %w", err)
		}
	}

	freq := p.FrequencyKHz
	if !isSat {
		freq = p.FrequencyKHz * 1000 // terrestrial/cable DTV_FREQUENCY is in Hz
	}

	props := []dtvProperty{
		newDTVProperty(dtvClear, 0),
		newDTVProperty(dtvDeliverySystem, delsys),
		newDTVProperty(dtvFrequency, freq),
		newDTVProperty(dtvInversion, inversionAuto),
	}
	switch p.System {
	case "DVBS", "DVBS2":
		props = append(props,
			newDTVProperty(dtvSymbolRate, p.SymbolRateKS*1000),
			newDTVProperty(dtvInnerFEC, fecAuto),
		)
	case "DVBT", "DVBT2":
		props = append(props, newDTVProperty(dtvBandwidthHz, p.BandwidthHz))
	case "DVBC":
		props = append(props,
			newDTVProperty(dtvSymbolRate, p.SymbolRateKS*1000),
			newDTVProperty(dtvModulation, modulationOf(p.Modulation)),
		)
	}
	props = append(props, newDTVProperty(dtvTune, 0))

	if err := fe.setProperties(props); err != nil {
		return fmt.Errorf("dvb: tune: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := fe.readStatus()
		if err != nil {
			return err
		}
		if status&feHasLock != 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("dvb: no lock after %s", timeout)
}

func (fe *Frontend) setProperties(props []dtvProperty) error {
	var dp dtvProperties
	dp.Num = uint32(len(props))
	dp.Props = uintptr(unsafe.Pointer(&props[0]))
	err := ioctlPtr(fe.f, feSetProperty, unsafe.Pointer(&dp))
	runtime.KeepAlive(props)
	return err
}

func (fe *Frontend) readStatus() (uint32, error) {
	var status uint32
	if err := ioctlPtr(fe.f, feReadStatus, unsafe.Pointer(&status)); err != nil {
		return 0, fmt.Errorf("dvb: read status: %w", err)
	}
	return status, nil
}
