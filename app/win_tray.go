//go:build windows

package main

import (
	"image"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procRegisterClassExW       = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW        = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW         = modUser32.NewProc("DefWindowProcW")
	procGetMessageW            = modUser32.NewProc("GetMessageW")
	procTranslateMessage       = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW       = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage        = modUser32.NewProc("PostQuitMessage")
	procPostMessageW           = modUser32.NewProc("PostMessageW")
	procCreatePopupMenu        = modUser32.NewProc("CreatePopupMenu")
	procAppendMenuW            = modUser32.NewProc("AppendMenuW")
	procTrackPopupMenu         = modUser32.NewProc("TrackPopupMenu")
	procDestroyMenu            = modUser32.NewProc("DestroyMenu")
	procSetForegroundWindow    = modUser32.NewProc("SetForegroundWindow")
	procGetCursorPos           = modUser32.NewProc("GetCursorPos")
	procRegisterWindowMessageW = modUser32.NewProc("RegisterWindowMessageW")
	procCreateIconIndirect     = modUser32.NewProc("CreateIconIndirect")
	procDestroyWindow          = modUser32.NewProc("DestroyWindow")
	procShellNotifyIconW       = modShell32.NewProc("Shell_NotifyIconW")
	modGdi32                   = syscall.NewLazyDLL("gdi32.dll")
	procCreateBitmap           = modGdi32.NewProc("CreateBitmap")
	procCreateDIBSection       = modGdi32.NewProc("CreateDIBSection")
)

const (
	wmDestroy     = 0x0002
	wmCommand     = 0x0111
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmApp         = 0x8000
	wmTray        = wmApp + 1
	wmUpdateTip   = wmApp + 2
	wmQuit        = wmApp + 3
	wmOpenUI      = wmApp + 4
	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nifMessage    = 1
	nifIcon       = 2
	nifTip        = 4
	mfString      = 0
	mfSeparator   = 0x800
	mfChecked     = 0x8
	tpmRightAlign = 0x8
	tpmBottomAlgn = 0x20
	tpmReturnCmd  = 0x100

	idOpen    = 1
	idTest    = 2
	idStartup = 3
	idQuit    = 4
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   syscall.Handle
	Icon       syscall.Handle
	Cursor     syscall.Handle
	Background syscall.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     syscall.Handle
}

type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        GUID
	BalloonIcon     uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	private uint32
}

type tray struct {
	hwnd           uintptr
	icon           uintptr
	taskbarCreated uint32
	onOpen         func(page string)
	mu             sync.Mutex
	tip            string
	nid            notifyIconData
}

var theTray *tray

func requestQuit() {
	if theTray != nil && theTray.hwnd != 0 {
		procPostMessageW.Call(theTray.hwnd, wmQuit, 0, 0)
	}
}

// SetTip updates the hover text from any goroutine.
func (t *tray) SetTip(s string) {
	t.mu.Lock()
	changed := t.tip != s
	t.tip = s
	t.mu.Unlock()
	if changed && t.hwnd != 0 {
		procPostMessageW.Call(t.hwnd, wmUpdateTip, 0, 0)
	}
}

func (t *tray) applyTip(flags uint32) {
	t.mu.Lock()
	tip := t.tip
	t.mu.Unlock()
	u, _ := syscall.UTF16FromString(tip)
	if len(u) > 127 {
		u = append(u[:127], 0)
	}
	t.nid.Tip = [128]uint16{}
	copy(t.nid.Tip[:], u)
	t.nid.Flags = flags
	op := uintptr(nimModify)
	if flags&nifIcon != 0 {
		op = nimAdd
	}
	procShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(&t.nid)))
}

func (t *tray) wndProc(hwnd, msgID, wp, lp uintptr) uintptr {
	m := uint32(msgID)
	switch {
	case m == wmTray:
		switch uint32(lp) & 0xFFFF {
		case wmLButtonUp:
			t.onOpen("")
		case wmRButtonUp:
			t.showMenu()
		}
		return 0
	case m == wmUpdateTip:
		t.applyTip(nifTip)
		return 0
	case m == wmCommand:
		switch wp & 0xFFFF {
		case idOpen:
			t.onOpen("")
		case idTest:
			t.onOpen("test")
		case idStartup:
			setStartup(!startupEnabled())
		case idQuit:
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case m == wmOpenUI:
		t.onOpen("")
		return 0
	case m == wmQuit:
		procDestroyWindow.Call(hwnd)
		return 0
	case m == wmDestroy:
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
		procPostQuitMessage.Call(0)
		return 0
	case t.taskbarCreated != 0 && m == t.taskbarCreated:
		t.applyTip(nifIcon | nifTip | nifMessage) // Explorer restarted: re-add the icon
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msgID, wp, lp)
	return r
}

func (t *tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	add := func(id uintptr, text string, flags uintptr) {
		p, _ := syscall.UTF16PtrFromString(text)
		procAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(p)))
	}
	add(idOpen, "Open Deej Mixer", mfString)
	add(idTest, "Hardware test", mfString)
	var st uintptr
	if startupEnabled() {
		st = mfChecked
	}
	add(idStartup, "Start with Windows", mfString|st)
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(idQuit, "Quit", mfString)
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightAlign|tpmBottomAlgn|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	procDestroyMenu.Call(menu)
	if cmd != 0 {
		t.wndProc(t.hwnd, wmCommand, cmd, 0)
	}
}

// makeHICON converts an RGBA image to an icon handle.
func makeHICON(img *image.RGBA) uintptr {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	type bitmapInfoHeader struct {
		Size                         uint32
		Width, Height                int32
		Planes, BitCount             uint16
		Compression, SizeImage       uint32
		XPels, YPels, ClrUsed, ClrIm uint32
	}
	bi := bitmapInfoHeader{Size: 40, Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	var bits unsafe.Pointer
	color, _, _ := procCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if color == 0 || bits == nil {
		return 0
	}
	px := unsafe.Slice((*byte)(bits), w*h*4)
	for i := 0; i < w*h; i++ {
		r, g, b, a := img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3]
		px[i*4], px[i*4+1], px[i*4+2], px[i*4+3] = b, g, r, a
	}
	mask, _, _ := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, 0)
	type iconInfo struct {
		FIcon             int32
		XHot, YHot        uint32
		HbmMask, HbmColor uintptr
	}
	ii := iconInfo{FIcon: 1, HbmMask: mask, HbmColor: color}
	hicon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return hicon
}

// runTray creates the notification-area icon and runs the Windows message loop until Quit.
func runTray(tip string, onOpen func(page string), openNow bool) {
	runtime.LockOSThread()
	defer comInit()() // WebView2 needs an STA thread with a message loop: this one
	t := &tray{onOpen: onOpen, tip: tip}
	theTray = t
	className, _ := syscall.UTF16PtrFromString("DeejMixerTrayWindow")
	wc := wndClassEx{WndProc: syscall.NewCallback(t.wndProc), ClassName: className}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	title, _ := syscall.UTF16PtrFromString("Deej Mixer")
	t.hwnd, _, _ = procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, 0, 0)
	tc, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tc)))
	t.taskbarCreated = uint32(r)
	t.icon = makeHICON(drawIcon(32))
	t.nid = notifyIconData{Wnd: t.hwnd, ID: 1, CallbackMessage: wmTray, Icon: t.icon}
	t.nid.Size = uint32(unsafe.Sizeof(t.nid))
	t.applyTip(nifIcon | nifTip | nifMessage)
	if openNow {
		procPostMessageW.Call(t.hwnd, wmOpenUI, 0, 0)
	}
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
