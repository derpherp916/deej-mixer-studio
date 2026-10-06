//go:build windows

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed firmware/DeejMixer.hex
var firmwareHex []byte

var (
	modUser32          = syscall.NewLazyDLL("user32.dll")
	procKeybdEvent     = modUser32.NewProc("keybd_event")
	procMessageBoxW    = modUser32.NewProc("MessageBoxW")
	modShell32         = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW  = modShell32.NewProc("ShellExecuteW")
	modAdvapi32        = syscall.NewLazyDLL("advapi32.dll")
	procRegSetValueExW = modAdvapi32.NewProc("RegSetValueExW")
	procRegDeleteValue = modAdvapi32.NewProc("RegDeleteValueW")
	procRegEnumValueW  = modAdvapi32.NewProc("RegEnumValueW")
	procRegCreateKeyEx = modAdvapi32.NewProc("RegCreateKeyExW")

	clsidShellLink  = mustGUID("{00021401-0000-0000-C000-000000000046}")
	iidIShellLinkW  = mustGUID("{000214F9-0000-0000-C000-000000000046}")
	iidIPersistFile = mustGUID("{0000010B-0000-0000-C000-000000000046}")
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "DeejMixer"

type winPlatform struct{}

func (winPlatform) ThreadInit() func()                 { return comInit() }
func (winPlatform) NewAudio() (AudioBackend, error)    { return newWinAudio() }
func (winPlatform) ListPorts() []PortInfo              { return listPorts() }
func (winPlatform) OpenPort(name string) (Port, error) { return openSerial(name) }

func (winPlatform) SendKeys(vks []uint16) error {
	const keyUp = 0x2
	for _, vk := range vks {
		procKeybdEvent.Call(uintptr(vk), 0, 0, 0)
	}
	time.Sleep(20 * time.Millisecond)
	for i := len(vks) - 1; i >= 0; i-- {
		procKeybdEvent.Call(uintptr(vks[i]), 0, keyUp, 0)
	}
	return nil
}

func shellOpen(target, args string) error { return shellExec("open", target, args) }

// shellExec runs ShellExecuteW; verb "runas" asks Windows for administrator rights (UAC prompt).
func shellExec(verbName, target, args string) error {
	verb, _ := syscall.UTF16PtrFromString(verbName)
	file, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	var a *uint16
	if args != "" {
		a, _ = syscall.UTF16PtrFromString(args)
	}
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(a)), 0, 1)
	if r <= 32 {
		return fmt.Errorf("Windows could not open %q (code %d)", target, r)
	}
	return nil
}

func (winPlatform) Launch(target string) error { return shellOpen(expandEnv(target), "") }

// UploadFirmware runs the bundled avrdude. New-bootloader Nanos use 115200 baud, old ones 57600.
func (winPlatform) UploadFirmware(port, customHex string, logf func(string)) error {
	avrdude := filepath.Join(exeDir(), "tools", "avrdude.exe")
	conf := filepath.Join(exeDir(), "tools", "avrdude.conf")
	if _, err := os.Stat(avrdude); err != nil {
		return errors.New("tools\\avrdude.exe is missing next to DeejMixer.exe (keep the whole folder together)")
	}
	hex := customHex
	if hex == "" {
		hex = filepath.Join(os.TempDir(), "DeejMixer-firmware.hex")
		if err := os.WriteFile(hex, firmwareHex, 0o644); err != nil {
			return err
		}
		defer os.Remove(hex)
	}
	for _, baud := range []string{"115200", "57600"} {
		logf(fmt.Sprintf("--- Trying %s at %s baud ---\n", port, baud))
		cmd := exec.Command(avrdude, "-C", conf, "-p", "atmega328p", "-c", "arduino", "-P", port, "-b", baud, "-D",
			"-U", "flash:w:"+hex+":i")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		out, err := runWithTimeout(cmd, 90*time.Second)
		logf(out)
		if err == nil {
			return nil
		}
	}
	return errors.New("the Nano did not respond. Check the port, close Arduino Serial Monitor, and try again")
}

func runWithTimeout(cmd *exec.Cmd, d time.Duration) (string, error) {
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return err.Error(), err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(d):
		cmd.Process.Kill()
		return buf.String() + "\n(timed out)", errors.New("timeout")
	}
}

// ---------- paths ----------

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

func dataDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base, _ = os.UserConfigDir()
	}
	return filepath.Join(base, "DeejMixer")
}

func installDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "DeejMixer")
}

func isInstalled() bool {
	a, _ := filepath.Abs(exeDir())
	b, _ := filepath.Abs(installDir())
	if strings.EqualFold(a, b) {
		return true
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} { // installed by DeejMixer-Setup.exe
		if pf := os.Getenv(env); pf != "" && strings.HasPrefix(strings.ToLower(a), strings.ToLower(pf)+`\`) {
			return true
		}
	}
	return false
}

// ---------- startup (HKCU Run) ----------

func regOpenRun(write bool) (syscall.Handle, error) {
	var h syscall.Handle
	sub, _ := syscall.UTF16PtrFromString(runKey)
	access := uint32(syscall.KEY_READ)
	if write {
		access = syscall.KEY_READ | syscall.KEY_WRITE
		r, _, _ := procRegCreateKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(sub)), 0, 0, 0,
			uintptr(access), 0, uintptr(unsafe.Pointer(&h)), 0)
		if r != 0 {
			return 0, syscall.Errno(r)
		}
		return h, nil
	}
	err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, sub, 0, access, &h)
	return h, err
}

func startupCommand() string { return fmt.Sprintf(`"%s" --background`, exePath()) }

func exePath() string { p, _ := os.Executable(); return p }

func startupEnabled() bool {
	h, err := regOpenRun(false)
	if err != nil {
		return false
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString(runValue)
	var typ, size uint32
	return syscall.RegQueryValueEx(h, name, nil, &typ, nil, &size) == nil && size > 0
}

func setStartup(on bool) error {
	h, err := regOpenRun(true)
	if err != nil {
		return err
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString(runValue)
	if !on {
		procRegDeleteValue.Call(uintptr(h), uintptr(unsafe.Pointer(name)))
		return nil
	}
	val, _ := syscall.UTF16FromString(startupCommand())
	r, _, _ := procRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, syscall.REG_SZ,
		uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// ---------- shortcuts & install ----------

// createShortcut writes a .lnk using IShellLinkW + IPersistFile on a dedicated COM thread.
func createShortcut(lnk, target, args, workdir, desc string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer comInit()()
		sl, err := coCreate(clsidShellLink, iidIShellLinkW)
		if err != nil {
			errc <- err
			return
		}
		defer sl.release()
		u := func(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
		pTarget, pArgs, pDir, pDesc := u(target), u(args), u(workdir), u(desc)
		sl.call(20, uintptr(unsafe.Pointer(pTarget)))    // SetPath
		sl.call(11, uintptr(unsafe.Pointer(pArgs)))      // SetArguments
		sl.call(9, uintptr(unsafe.Pointer(pDir)))        // SetWorkingDirectory
		sl.call(7, uintptr(unsafe.Pointer(pDesc)))       // SetDescription
		sl.call(17, uintptr(unsafe.Pointer(pTarget)), 0) // SetIconLocation
		pf, err := sl.query(iidIPersistFile)
		if err != nil {
			errc <- err
			return
		}
		defer pf.release()
		pLnk := u(lnk)
		if hr := pf.call(6, uintptr(unsafe.Pointer(pLnk)), 1); failed(hr) { // Save
			errc <- hrErr("saving shortcut", hr)
			return
		}
		errc <- nil
	}()
	return <-errc
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Remove(dst)
	return os.Rename(tmp, dst)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// install copies the app (and its tools/drivers) into %LOCALAPPDATA%\Programs\DeejMixer,
// creates Start-menu and desktop shortcuts and turns on start-with-Windows.
func install() (string, error) {
	dst := installDir()
	if isInstalled() {
		if err := setStartup(true); err != nil {
			return "", err
		}
		return "Already installed. Start-with-Windows is on.", nil
	}
	if err := copyFile(exePath(), filepath.Join(dst, "DeejMixer.exe")); err != nil {
		return "", fmt.Errorf("copy failed (close any other copy of Deej Mixer): %v", err)
	}
	for _, sub := range []string{"tools", "drivers", "firmware"} {
		if _, err := os.Stat(filepath.Join(exeDir(), sub)); err == nil {
			if err := copyTree(filepath.Join(exeDir(), sub), filepath.Join(dst, sub)); err != nil {
				return "", err
			}
		}
	}
	target := filepath.Join(dst, "DeejMixer.exe")
	menu := filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs\Deej Mixer.lnk`)
	if err := createShortcut(menu, target, "", dst, "Deej Mixer volume controller"); err != nil {
		return "", err
	}
	if home, err := os.UserHomeDir(); err == nil {
		desk := filepath.Join(home, "Desktop", "Deej Mixer.lnk")
		createShortcut(desk, target, "", dst, "Deej Mixer volume controller")
	}
	// Point start-with-Windows at the installed copy.
	h, err := regOpenRun(true)
	if err == nil {
		name, _ := syscall.UTF16PtrFromString(runValue)
		val, _ := syscall.UTF16FromString(fmt.Sprintf(`"%s" --background`, target))
		procRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, syscall.REG_SZ, uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
		syscall.RegCloseKey(h)
	}
	// Hand over to the installed copy.
	cmd := exec.Command(target, "--after-install", fmt.Sprint(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return "Installed. Start it from the Start menu.", nil
	}
	go func() { time.Sleep(700 * time.Millisecond); requestQuit() }()
	return "Installed! Deej Mixer now runs from the Start menu and starts with Windows. This window will reopen from the installed copy.", nil
}

// openDrivers adds the bundled, WCH-signed CH340/CH341 driver package to the Windows driver store
// (pnputil, with a UAC prompt). Windows then loads it automatically whenever the mixer is plugged in.
func openDrivers() error {
	inf := filepath.Join(exeDir(), "drivers", "ch341", "CH341SER.INF")
	if _, err := os.Stat(inf); err == nil {
		return shellExec("runas", filepath.Join(os.Getenv("SystemRoot"), "System32", "pnputil.exe"),
			fmt.Sprintf(`/add-driver "%s" /install`, inf))
	}
	p := filepath.Join(exeDir(), "drivers", "CH341SER.EXE")
	if _, err := os.Stat(p); err != nil {
		return shellOpen("https://www.wch-ic.com/downloads/CH341SER_EXE.html", "")
	}
	return shellOpen(p, "")
}

func messageBox(text, title string, flags uintptr) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
}

// singleInstanceFree reports whether no running copy holds the mutex (without taking it).
func singleInstanceFree() bool {
	name, _ := syscall.UTF16PtrFromString(`Local\DeejMixerV2`)
	const synchronize = 0x00100000
	h, _, _ := procOpenMutexW.Call(synchronize, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return true
	}
	syscall.CloseHandle(syscall.Handle(h))
	return false
}

// singleInstance returns false if another copy already holds the mutex.
func singleInstance() bool {
	name, _ := syscall.UTF16PtrFromString(`Local\DeejMixerV2`)
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return true
	}
	return err != syscall.ERROR_ALREADY_EXISTS
}

func waitForPID(pid int, d time.Duration) {
	const synchronize = 0x00100000
	h, err := syscall.OpenProcess(synchronize, false, uint32(pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)
	syscall.WaitForSingleObject(h, uint32(d/time.Millisecond))
}
