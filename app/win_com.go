//go:build windows

package main

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	modOle32             = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = modOle32.NewProc("CoInitializeEx")
	procCoUninitialize   = modOle32.NewProc("CoUninitialize")
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = modOle32.NewProc("CoTaskMemFree")
)

const (
	clsctxAll            = 0x17
	coinitApartmentThred = 0x2
)

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// mustGUID parses "{XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX}".
func mustGUID(s string) *GUID {
	s = strings.Trim(s, "{}")
	p := strings.Split(s, "-")
	if len(p) != 5 || len(p[3]) != 4 || len(p[4]) != 12 {
		panic("bad guid " + s)
	}
	d1, _ := strconv.ParseUint(p[0], 16, 32)
	d2, _ := strconv.ParseUint(p[1], 16, 16)
	d3, _ := strconv.ParseUint(p[2], 16, 16)
	g := &GUID{Data1: uint32(d1), Data2: uint16(d2), Data3: uint16(d3)}
	rest := p[3] + p[4]
	for i := 0; i < 8; i++ {
		b, _ := strconv.ParseUint(rest[i*2:i*2+2], 16, 8)
		g.Data4[i] = byte(b)
	}
	return g
}

// com wraps a COM interface pointer.
type com struct{ p unsafe.Pointer }

func (c com) valid() bool { return c.p != nil }

// call invokes vtable method `method`. Pointer arguments must be converted to uintptr directly in the
// call expression; go:uintptrescapes then keeps their targets on the heap and alive for the call.
//
//go:uintptrescapes
func (c com) call(method int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(c.p)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(method)*unsafe.Sizeof(uintptr(0))))
	all := append([]uintptr{uintptr(c.p)}, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func (c *com) release() {
	if c.p != nil {
		c.call(2)
		c.p = nil
	}
}

func (c com) query(iid *GUID) (com, error) {
	var out unsafe.Pointer
	if hr := c.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); failed(hr) {
		return com{}, hrErr("QueryInterface", hr)
	}
	return com{out}, nil
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

func hrErr(what string, hr uintptr) error { return fmt.Errorf("%s failed (0x%08X)", what, uint32(hr)) }

func coCreate(clsid, iid *GUID) (com, error) {
	var out unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(clsid)), 0, clsctxAll, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if failed(hr) {
		return com{}, hrErr("CoCreateInstance", hr)
	}
	return com{out}, nil
}

// takeString copies and frees a CoTaskMem-allocated UTF-16 string.
func takeString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(p))
	return utf16PtrToString((*uint16)(p))
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var buf []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		ch := *(*uint16)(ptr)
		if ch == 0 {
			break
		}
		buf = append(buf, ch)
		if len(buf) > 32768 {
			break
		}
	}
	return syscall.UTF16ToString(buf)
}

func comInit() func() {
	hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThred)
	if failed(hr) {
		return func() {}
	}
	return func() { procCoUninitialize.Call() }
}
