//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"
)

var (
	modKernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procQueryFullProcessImageNameW = modKernel32.NewProc("QueryFullProcessImageNameW")
	procCreateMutexW               = modKernel32.NewProc("CreateMutexW")
	procOpenMutexW                 = modKernel32.NewProc("OpenMutexW")
	procExpandEnvironmentStringsW  = modKernel32.NewProc("ExpandEnvironmentStringsW")
	procGetCommState               = modKernel32.NewProc("GetCommState")
	procSetCommState               = modKernel32.NewProc("SetCommState")
	procSetCommTimeouts            = modKernel32.NewProc("SetCommTimeouts")
	procPurgeComm                  = modKernel32.NewProc("PurgeComm")
	procEscapeCommFunction         = modKernel32.NewProc("EscapeCommFunction")
)

const processQueryLimitedInformation = 0x1000

func processPath(pid uint32) string {
	p, _ := processDetails(pid)
	return p
}

// processDetails returns a process's exe path and creation time with one handle.
func processDetails(pid uint32) (string, int64) {
	if pid == 0 {
		return "", 0
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return "", 0
	}
	defer syscall.CloseHandle(h)
	path := ""
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if r, _, _ := procQueryFullProcessImageNameW.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		path = syscall.UTF16ToString(buf[:size])
	}
	var c, e, k, u syscall.Filetime
	var created int64
	if syscall.GetProcessTimes(h, &c, &e, &k, &u) == nil {
		created = c.Nanoseconds()
	}
	return path, created
}

// listProcesses returns every running process with its exe path and start time.
func listProcesses() []ProcInfo {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []ProcInfo
	for err = syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		pid := pe.ProcessID
		if pid == 0 || pid == 4 {
			continue
		}
		name := strings.ToLower(syscall.UTF16ToString(pe.ExeFile[:]))
		path, created := processDetails(pid)
		out = append(out, ProcInfo{PID: pid, Name: name, Path: strings.ToLower(path), Created: created})
	}
	return out
}

func expandEnv(s string) string {
	src, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return s
	}
	buf := make([]uint16, 4096)
	n, _, _ := procExpandEnvironmentStringsW.Call(uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) > len(buf) {
		return s
	}
	return syscall.UTF16ToString(buf)
}
