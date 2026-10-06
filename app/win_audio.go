//go:build windows

package main

import (
	"fmt"
	"math"
	"strings"
	"unsafe"
)

var (
	clsidMMDeviceEnumerator  = mustGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidIMMDeviceEnumerator   = mustGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidIAudioEndpointVolume  = mustGUID("{5CDF2C82-841E-4546-9722-0CF74078229A}")
	iidIAudioSessionManager2 = mustGUID("{77AA99A0-1BD6-484F-8BC7-2C654C9A9B6F}")
	iidIAudioSessionControl2 = mustGUID("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")
	iidISimpleAudioVolume    = mustGUID("{87CE5498-68D6-44E5-9215-6DA47EF883D8}")
)

const (
	eRender             = 0
	eCapture            = 1
	eConsole            = 0
	deviceStateActive   = 1
	sessionStateExpired = 2
)

// vtable indexes
const (
	mmdeEnumAudioEndpoints    = 3
	mmdeGetDefaultEndpoint    = 4
	mmdcGetCount              = 3
	mmdcItem                  = 4
	mmdActivate               = 3
	aevSetMasterScalar        = 7
	aevSetMute                = 14
	aevGetMute                = 15
	asm2GetSessionEnumerator  = 5
	aseGetCount               = 3
	aseGetSession             = 4
	ascGetState               = 3
	asc2GetSessionInstanceID  = 13
	asc2GetProcessID          = 14
	asc2IsSystemSoundsSession = 15
	savSetMasterVolume        = 3
	savSetMute                = 5
	savGetMute                = 6
)

// endpointVol implements VolumeCtl for a whole device (speakers or microphone).
type endpointVol struct{ c com }

func (v *endpointVol) SetVolume(f float32) error {
	if hr := v.c.call(aevSetMasterScalar, uintptr(math.Float32bits(f)), 0); failed(hr) {
		return hrErr("SetMasterVolumeLevelScalar", hr)
	}
	return nil
}
func (v *endpointVol) Muted() (bool, error) {
	var b int32
	if hr := v.c.call(aevGetMute, uintptr(unsafe.Pointer(&b))); failed(hr) {
		return false, hrErr("GetMute", hr)
	}
	return b != 0, nil
}
func (v *endpointVol) SetMute(m bool) error {
	if hr := v.c.call(aevSetMute, boolArg(m), 0); failed(hr) {
		return hrErr("SetMute", hr)
	}
	return nil
}

// sessionVol implements VolumeCtl for one app's audio session.
type sessionVol struct{ c com }

func (v *sessionVol) SetVolume(f float32) error {
	if hr := v.c.call(savSetMasterVolume, uintptr(math.Float32bits(f)), 0); failed(hr) {
		return hrErr("SetMasterVolume", hr)
	}
	return nil
}
func (v *sessionVol) Muted() (bool, error) {
	var b int32
	if hr := v.c.call(savGetMute, uintptr(unsafe.Pointer(&b))); failed(hr) {
		return false, hrErr("GetMute", hr)
	}
	return b != 0, nil
}
func (v *sessionVol) SetMute(m bool) error {
	if hr := v.c.call(savSetMute, boolArg(m), 0); failed(hr) {
		return hrErr("SetMute", hr)
	}
	return nil
}

func boolArg(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

type winAudio struct {
	enum  com
	owned []com // released on the next refresh
}

func newWinAudio() (*winAudio, error) {
	e, err := coCreate(clsidMMDeviceEnumerator, iidIMMDeviceEnumerator)
	if err != nil {
		return nil, err
	}
	return &winAudio{enum: e}, nil
}

func (a *winAudio) keep(c com) com { a.owned = append(a.owned, c); return c }

func (a *winAudio) releaseAll() {
	for i := range a.owned {
		a.owned[i].release()
	}
	a.owned = nil
}

func (a *winAudio) Close() {
	a.releaseAll()
	a.enum.release()
}

func (a *winAudio) defaultEndpointVolume(flow uintptr) VolumeCtl {
	var dev unsafe.Pointer
	if hr := a.enum.call(mmdeGetDefaultEndpoint, flow, eConsole, uintptr(unsafe.Pointer(&dev))); failed(hr) || dev == nil {
		return nil
	}
	d := com{dev}
	defer d.release()
	var ep unsafe.Pointer
	if hr := d.call(mmdActivate, uintptr(unsafe.Pointer(iidIAudioEndpointVolume)), clsctxAll, 0, uintptr(unsafe.Pointer(&ep))); failed(hr) || ep == nil {
		return nil
	}
	return &endpointVol{a.keep(com{ep})}
}

func (a *winAudio) Refresh() (*AudioSnapshot, error) {
	a.releaseAll()
	snap := &AudioSnapshot{Procs: listProcesses()}
	if m := a.defaultEndpointVolume(eRender); m != nil {
		snap.Master = m
	}
	if m := a.defaultEndpointVolume(eCapture); m != nil {
		snap.Mic = m
	}
	byPID := map[uint32]ProcInfo{}
	for _, p := range snap.Procs {
		byPID[p.PID] = p
	}

	var coll unsafe.Pointer
	if hr := a.enum.call(mmdeEnumAudioEndpoints, eRender, deviceStateActive, uintptr(unsafe.Pointer(&coll))); failed(hr) {
		return snap, hrErr("EnumAudioEndpoints", hr)
	}
	devices := com{coll}
	defer devices.release()
	var count uint32
	devices.call(mmdcGetCount, uintptr(unsafe.Pointer(&count)))
	seen := map[string]bool{}
	for di := uint32(0); di < count && di < 32; di++ {
		var dev unsafe.Pointer
		if hr := devices.call(mmdcItem, uintptr(di), uintptr(unsafe.Pointer(&dev))); failed(hr) || dev == nil {
			continue
		}
		d := com{dev}
		var mgrP unsafe.Pointer
		hr := d.call(mmdActivate, uintptr(unsafe.Pointer(iidIAudioSessionManager2)), clsctxAll, 0, uintptr(unsafe.Pointer(&mgrP)))
		d.release()
		if failed(hr) || mgrP == nil {
			continue
		}
		mgr := com{mgrP}
		a.addSessions(mgr, byPID, seen, snap)
		mgr.release()
	}
	return snap, nil
}

func (a *winAudio) addSessions(mgr com, byPID map[uint32]ProcInfo, seen map[string]bool, snap *AudioSnapshot) {
	var enumP unsafe.Pointer
	if hr := mgr.call(asm2GetSessionEnumerator, uintptr(unsafe.Pointer(&enumP))); failed(hr) || enumP == nil {
		return
	}
	se := com{enumP}
	defer se.release()
	var n int32
	se.call(aseGetCount, uintptr(unsafe.Pointer(&n)))
	for i := int32(0); i < n && i < 512; i++ {
		var ctlP unsafe.Pointer
		if hr := se.call(aseGetSession, uintptr(i), uintptr(unsafe.Pointer(&ctlP))); failed(hr) || ctlP == nil {
			continue
		}
		ctl := com{ctlP}
		info, vol, ok := a.describe(ctl, byPID)
		ctl.release()
		if !ok {
			continue
		}
		if seen[info.Key] {
			vol.release()
			continue
		}
		seen[info.Key] = true
		snap.Sessions = append(snap.Sessions, info)
		snap.Ctls = append(snap.Ctls, &sessionVol{a.keep(vol)})
	}
}

func (a *winAudio) describe(ctl com, byPID map[uint32]ProcInfo) (SessionInfo, com, bool) {
	var state int32
	if hr := ctl.call(ascGetState, uintptr(unsafe.Pointer(&state))); failed(hr) || state == sessionStateExpired {
		return SessionInfo{}, com{}, false
	}
	c2, err := ctl.query(iidIAudioSessionControl2)
	if err != nil {
		return SessionInfo{}, com{}, false
	}
	defer c2.release()
	var info SessionInfo
	var idP unsafe.Pointer
	if hr := c2.call(asc2GetSessionInstanceID, uintptr(unsafe.Pointer(&idP))); !failed(hr) {
		info.Key = takeString(idP)
	}
	var pid uint32
	c2.call(asc2GetProcessID, uintptr(unsafe.Pointer(&pid)))
	info.PID = pid
	info.System = c2.call(asc2IsSystemSoundsSession) == 0 // S_OK means yes
	if p, ok := byPID[pid]; ok && !info.System {
		info.Name, info.Path = p.Name, p.Path
	} else if !info.System {
		info.Path = strings.ToLower(processPath(pid))
		info.Name = baseName(info.Path)
	}
	if info.Key == "" {
		info.Key = fmt.Sprintf("pid:%d", pid)
	}
	vol, err := ctl.query(iidISimpleAudioVolume)
	if err != nil {
		return SessionInfo{}, com{}, false
	}
	return info, vol, true
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}
