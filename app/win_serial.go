//go:build windows

package main

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

type dcb struct {
	DCBlength  uint32
	BaudRate   uint32
	Flags      uint32
	wReserved  uint16
	XonLim     uint16
	XoffLim    uint16
	ByteSize   byte
	Parity     byte
	StopBits   byte
	XonChar    byte
	XoffChar   byte
	ErrorChar  byte
	EofChar    byte
	EvtChar    byte
	wReserved1 uint16
}

type commTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

type winPort struct {
	h      syscall.Handle
	mu     sync.Mutex
	closed bool
}

func openSerial(name string) (Port, error) {
	if !strings.HasPrefix(strings.ToUpper(name), "COM") {
		return nil, errors.New("not a COM port")
	}
	path, _ := syscall.UTF16PtrFromString(`\\.\` + name)
	h, err := syscall.CreateFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	var d dcb
	d.DCBlength = uint32(unsafe.Sizeof(d))
	if r, _, e := procGetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, e
	}
	d.BaudRate = 115200
	d.ByteSize = 8
	d.Parity = 0
	d.StopBits = 0
	// fBinary | fDtrControl=ENABLE | fRtsControl=ENABLE; no flow control, no parity, no abort-on-error.
	d.Flags = 0x0001 | 0x0010 | 0x1000
	if r, _, e := procSetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, e
	}
	// Return as soon as any byte arrives, or after 200 ms with nothing.
	t := commTimeouts{ReadIntervalTimeout: 0xFFFFFFFF, ReadTotalTimeoutMultiplier: 0xFFFFFFFF, ReadTotalTimeoutConstant: 200,
		WriteTotalTimeoutConstant: 500}
	procSetCommTimeouts.Call(uintptr(h), uintptr(unsafe.Pointer(&t)))
	procPurgeComm.Call(uintptr(h), 0x0004|0x0008) // PURGE_TXCLEAR | PURGE_RXCLEAR
	procEscapeCommFunction.Call(uintptr(h), 5)    // SETDTR
	return &winPort{h: h}, nil
}

var errPortClosed = errors.New("port closed")

func (p *winPort) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return 0, errPortClosed
		}
		var n uint32
		if err := syscall.ReadFile(p.h, b, &n, nil); err != nil {
			return 0, err
		}
		if n > 0 {
			return int(n), nil
		}
	}
}

func (p *winPort) Write(b []byte) (int, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return 0, errPortClosed
	}
	var n uint32
	err := syscall.WriteFile(p.h, b, &n, nil)
	return int(n), err
}

func (p *winPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return syscall.CloseHandle(p.h)
}

var (
	modSetupapi                           = syscall.NewLazyDLL("setupapi.dll")
	procSetupDiGetClassDevsW              = modSetupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInfo             = modSetupapi.NewProc("SetupDiEnumDeviceInfo")
	procSetupDiGetDeviceRegistryPropertyW = modSetupapi.NewProc("SetupDiGetDeviceRegistryPropertyW")
	procSetupDiDestroyDeviceInfoList      = modSetupapi.NewProc("SetupDiDestroyDeviceInfoList")
	guidDevClassPorts                     = mustGUID("{4D36E978-E325-11CE-BFC1-08002BE10318}")
)

type spDevinfoData struct {
	CbSize    uint32
	ClassGUID GUID
	DevInst   uint32
	Reserved  uintptr
}

// friendlyPortNames maps "COM5" -> "USB-SERIAL CH340 (COM5)" using SetupAPI's Ports class.
func friendlyPortNames() map[string]string {
	out := map[string]string{}
	const digcfPresent = 0x2
	h, _, _ := procSetupDiGetClassDevsW.Call(uintptr(unsafe.Pointer(guidDevClassPorts)), 0, 0, digcfPresent)
	if h == 0 || h == ^uintptr(0) {
		return out
	}
	defer procSetupDiDestroyDeviceInfoList.Call(h)
	for i := uintptr(0); i < 256; i++ {
		var d spDevinfoData
		d.CbSize = uint32(unsafe.Sizeof(d))
		if r, _, _ := procSetupDiEnumDeviceInfo.Call(h, i, uintptr(unsafe.Pointer(&d))); r == 0 {
			break
		}
		buf := make([]uint16, 512)
		var typ, need uint32
		const spdrpFriendlyName = 0x0C
		r, _, _ := procSetupDiGetDeviceRegistryPropertyW.Call(h, uintptr(unsafe.Pointer(&d)), spdrpFriendlyName,
			uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&need)))
		if r == 0 {
			continue
		}
		name := syscall.UTF16ToString(buf)
		if a := strings.LastIndex(name, "(COM"); a >= 0 {
			if b := strings.Index(name[a:], ")"); b > 0 {
				out[strings.ToUpper(name[a+1:a+b])] = name
			}
		}
	}
	return out
}

// listPorts combines HKLM\HARDWARE\DEVICEMAP\SERIALCOMM (every live COM port) with SetupAPI friendly names.
func listPorts() []PortInfo {
	names := friendlyPortNames()
	seen := map[string]bool{}
	var ports []PortInfo
	add := func(port, desc string) {
		port = strings.ToUpper(strings.TrimSpace(port))
		if !strings.HasPrefix(port, "COM") || seen[port] {
			return
		}
		seen[port] = true
		if d, ok := names[port]; ok {
			desc = d
		}
		ports = append(ports, PortInfo{Name: port, Desc: desc})
	}
	var key syscall.Handle
	sub, _ := syscall.UTF16PtrFromString(`HARDWARE\DEVICEMAP\SERIALCOMM`)
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, sub, 0, syscall.KEY_READ, &key) == nil {
		for i := uint32(0); i < 256; i++ {
			name := make([]uint16, 256)
			nameLen := uint32(len(name))
			data := make([]uint16, 64)
			dataLen := uint32(len(data) * 2)
			var typ uint32
			r, _, _ := procRegEnumValueW.Call(uintptr(key), uintptr(i), uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)),
				0, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&dataLen)))
			if r != 0 {
				break
			}
			dev := syscall.UTF16ToString(name[:nameLen])
			desc := dev
			if strings.Contains(strings.ToLower(dev), "bth") {
				desc = "Bluetooth " + dev
			}
			add(syscall.UTF16ToString(data[:dataLen/2]), desc)
		}
		syscall.RegCloseKey(key)
	}
	for p, d := range names {
		add(p, d)
	}
	sort.Slice(ports, func(a, b int) bool {
		na, _ := strconv.Atoi(strings.TrimPrefix(ports[a].Name, "COM"))
		nb, _ := strconv.Atoi(strings.TrimPrefix(ports[b].Name, "COM"))
		return na < nb
	})
	return ports
}
