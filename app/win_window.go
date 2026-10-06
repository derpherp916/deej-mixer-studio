//go:build windows

package main

// A native "Deej Mixer" window that hosts the control panel with Microsoft Edge WebView2 (built into
// Windows 10/11). The WebView2 runtime is loaded directly (no WebView2Loader.dll needed). If WebView2 is
// missing or fails, Edge is opened in app mode (its own window, no tabs or address bar), and as a last
// resort the default browser.

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procShowWindow       = modUser32.NewProc("ShowWindow")
	procGetClientRect    = modUser32.NewProc("GetClientRect")
	procLoadCursorW      = modUser32.NewProc("LoadCursorW")
	procLoadIconW        = modUser32.NewProc("LoadIconW")
	procFindWindowW      = modUser32.NewProc("FindWindowW")
	procGetDpiForSystem  = modUser32.NewProc("GetDpiForSystem")
	procSetTimer         = modUser32.NewProc("SetTimer")
	procKillTimer        = modUser32.NewProc("KillTimer")
	procIsIconic         = modUser32.NewProc("IsIconic")
	procGetModuleHandleW = modKernel32.NewProc("GetModuleHandleW")
	procSendMessageW     = modUser32.NewProc("SendMessageW")
	procSetWindowTextW   = modUser32.NewProc("SetWindowTextW")
	procGetSystemMetrics = modUser32.NewProc("GetSystemMetrics")
	procSetWindowPos     = modUser32.NewProc("SetWindowPos")
	procBringWindowToTop = modUser32.NewProc("BringWindowToTop")
	procAllowSetFgWindow = modUser32.NewProc("AllowSetForegroundWindow")
	procDwmSetWindowAttr = syscall.NewLazyDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	procCreateSolidBrush = modGdi32.NewProc("CreateSolidBrush")

	iidWebView2Settings3   = mustGUID("{FDB5AB74-AF33-4854-84F0-0A631DEB5EBA}")
	iidWebView2Controller2 = mustGUID("{C979903E-D4CA-4228-92EB-47EE3FA96EAB}")
)

const (
	wmSize         = 0x0005
	wmSetFocus     = 0x0007
	wmClose        = 0x0010
	wmTimer        = 0x0113
	wmSetIcon      = 0x0080
	wmGetMinMax    = 0x0024
	appBgColorRef  = 0x00120D0B // #0B0D12 as COLORREF (0x00BBGGRR)
	wsOverlappedWn = 0x00CF0000
	cwUseDefault   = 0x80000000
	swHide         = 0
	swShow         = 5
	swRestore      = 9
	timerFallback  = 1

	wvStateNone     = 0
	wvStateCreating = 1
	wvStateReady    = 2
	wvStateFailed   = 3
)

// ---------- minimal COM callback objects (completion handlers) ----------

type comHandler struct {
	vtbl *[4]uintptr
	fn   func(hr uintptr, obj unsafe.Pointer)
}

var (
	handlerVtbl [4]uintptr
	liveHandlrs []*comHandler // referenced so the GC never frees objects WebView2 still holds
)

func init() {
	handlerVtbl = [4]uintptr{
		syscall.NewCallback(func(this *comHandler, riid unsafe.Pointer, out *unsafe.Pointer) uintptr {
			*out = unsafe.Pointer(this)
			return 0
		}),
		syscall.NewCallback(func(this *comHandler) uintptr { return 1 }),
		syscall.NewCallback(func(this *comHandler) uintptr { return 1 }),
		syscall.NewCallback(func(this *comHandler, hr uintptr, obj unsafe.Pointer) uintptr {
			this.fn(hr, obj)
			return 0
		}),
	}
}

func newHandler(fn func(hr uintptr, obj unsafe.Pointer)) *comHandler {
	h := &comHandler{vtbl: &handlerVtbl, fn: fn}
	liveHandlrs = append(liveHandlrs, h)
	return h
}

// ---------- the window ----------

type uiWindow struct {
	hwnd       uintptr
	state      int
	controller com
	webview    com
	baseURL    string
	pending    string
}

var ui uiWindow

type rect struct{ Left, Top, Right, Bottom int32 }

// findWebView2 returns the newest EmbeddedBrowserWebView.dll from the WebView2 runtime (or Edge).
func findWebView2() string {
	var roots []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		if b := os.Getenv(env); b != "" {
			roots = append(roots, filepath.Join(b, "Microsoft", "EdgeWebView", "Application"),
				filepath.Join(b, "Microsoft", "Edge", "Application"))
		}
	}
	type cand struct {
		path string
		ver  []int
	}
	var cands []cand
	for _, r := range roots {
		matches, _ := filepath.Glob(filepath.Join(r, "*", "EBWebView", "x64", "EmbeddedBrowserWebView.dll"))
		for _, m := range matches {
			verDir := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(m))))
			var v []int
			for _, part := range strings.Split(verDir, ".") {
				n, err := strconv.Atoi(part)
				if err != nil {
					v = nil
					break
				}
				v = append(v, n)
			}
			if v != nil {
				cands = append(cands, cand{m, v})
			}
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].ver, cands[j].ver
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return len(a) > len(b)
	})
	return cands[0].path
}

func (w *uiWindow) wndProc(hwnd, msgID, wp, lp uintptr) uintptr {
	switch uint32(msgID) {
	case wmSize:
		w.resize()
		return 0
	case wmSetFocus:
		if w.controller.valid() {
			w.controller.call(12, 0) // MoveFocus(PROGRAMMATIC)
		}
	case wmGetMinMax:
		type point struct{ X, Y int32 }
		type minMaxInfo struct{ Reserved, MaxSize, MaxPosition, MinTrackSize, MaxTrackSize point }
		mmi := (*minMaxInfo)(unsafe.Pointer(lp))
		dpi := int32(96)
		if procGetDpiForSystem.Find() == nil {
			if d, _, _ := procGetDpiForSystem.Call(); d > 0 {
				dpi = int32(d)
			}
		}
		mmi.MinTrackSize = point{1000 * dpi / 96, 680 * dpi / 96}
		return 0
	case wmClose:
		procShowWindow.Call(hwnd, swHide) // keep running in the tray
		return 0
	case wmTimer:
		if wp == timerFallback {
			procKillTimer.Call(hwnd, timerFallback)
			if w.state != wvStateReady {
				w.fail("WebView2 did not start within 15 s")
			}
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msgID, wp, lp)
	return r
}

func (w *uiWindow) resize() {
	if !w.controller.valid() || w.hwnd == 0 {
		return
	}
	var r rect
	procGetClientRect.Call(w.hwnd, uintptr(unsafe.Pointer(&r)))
	w.controller.call(6, uintptr(unsafe.Pointer(&r))) // put_Bounds
}

func (w *uiWindow) fail(why string) {
	log.Printf("Native window unavailable (%s); using Edge app window instead", why)
	w.state = wvStateFailed
	if w.hwnd != 0 {
		procDestroyWindow.Call(w.hwnd)
		w.hwnd = 0
	}
	openAppWindow(w.pending)
}

func (w *uiWindow) create() bool {
	inst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("DeejMixerWindow")
	icon, _, _ := procLoadIconW.Call(inst, 1) // icon resource #1 from the exe
	cursor, _, _ := procLoadCursorW.Call(0, 32512)
	brush, _, _ := procCreateSolidBrush.Call(appBgColorRef)
	wc := wndClassEx{WndProc: syscall.NewCallback(w.wndProc), ClassName: className, Instance: syscall.Handle(inst),
		Icon: syscall.Handle(icon), IconSm: syscall.Handle(icon), Cursor: syscall.Handle(cursor), Background: syscall.Handle(brush)}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	dpi := uintptr(96)
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d > 0 {
			dpi = d
		}
	}
	width, height := 1180*dpi/96, 880*dpi/96
	sw, _, _ := procGetSystemMetrics.Call(0)
	sh, _, _ := procGetSystemMetrics.Call(1)
	if sw > 0 && width > sw-40 {
		width = sw - 40
	}
	if sh > 0 && height > sh-80 {
		height = sh - 80
	}
	x, y := uintptr(cwUseDefault), uintptr(cwUseDefault)
	if sw > width && sh > height {
		x, y = (sw-width)/2, (sh-height)/2
	}
	title, _ := syscall.UTF16PtrFromString("Deej Mixer")
	w.hwnd, _, _ = procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		wsOverlappedWn, x, y, width, height, 0, 0, inst, 0)
	if w.hwnd == 0 {
		return false
	}
	dark := int32(1) // dark title bar to match the panel (Windows 10 20H1+/11; ignored elsewhere)
	procDwmSetWindowAttr.Call(w.hwnd, 20, uintptr(unsafe.Pointer(&dark)), 4)
	caption, text := uint32(appBgColorRef), uint32(0x00EEE9E7) // Windows 11: title bar blends into the app
	procDwmSetWindowAttr.Call(w.hwnd, 35, uintptr(unsafe.Pointer(&caption)), 4)
	procDwmSetWindowAttr.Call(w.hwnd, 36, uintptr(unsafe.Pointer(&text)), 4)
	if icon != 0 {
		procSendMessageW.Call(w.hwnd, wmSetIcon, 0, icon)
		procSendMessageW.Call(w.hwnd, wmSetIcon, 1, icon)
	}
	return true
}

func (w *uiWindow) startWebView() {
	dll := findWebView2()
	if dll == "" {
		w.fail("WebView2 runtime not found")
		return
	}
	proc := syscall.NewLazyDLL(dll).NewProc("CreateWebViewEnvironmentWithOptionsInternal")
	if err := proc.Find(); err != nil {
		w.fail("WebView2 entry point missing: " + err.Error())
		return
	}
	userData, _ := syscall.UTF16PtrFromString(filepath.Join(dataDir(), "WebView2"))
	envDone := newHandler(func(hr uintptr, env unsafe.Pointer) {
		if failed(hr) || env == nil {
			w.fail(hrErr("creating WebView2 environment", hr).Error())
			return
		}
		ctrlDone := newHandler(func(hr uintptr, ctrl unsafe.Pointer) {
			if failed(hr) || ctrl == nil {
				w.fail(hrErr("creating WebView2 controller", hr).Error())
				return
			}
			w.controller = com{ctrl}
			w.controller.call(1) // AddRef: keep it after the callback returns
			var wv unsafe.Pointer
			if hr := w.controller.call(25, uintptr(unsafe.Pointer(&wv))); failed(hr) || wv == nil {
				w.fail(hrErr("get_CoreWebView2", hr).Error())
				return
			}
			w.webview = com{wv}
			w.makeAppLike()
			w.state = wvStateReady
			procKillTimer.Call(w.hwnd, timerFallback)
			w.resize()
			w.controller.call(4, 1) // put_IsVisible(TRUE)
			w.navigate(w.pending)
			log.Print("Native window ready (WebView2)")
		})
		com{env}.call(3, w.hwnd, uintptr(unsafe.Pointer(ctrlDone))) // CreateCoreWebView2Controller
	})
	procSetTimer.Call(w.hwnd, timerFallback, 15000, 0)
	hr, _, _ := proc.Call(1, 0, uintptr(unsafe.Pointer(userData)), 0, uintptr(unsafe.Pointer(envDone)))
	if failed(hr) {
		w.fail(hrErr("CreateWebViewEnvironment", hr).Error())
	}
}

func (w *uiWindow) navigate(url string) {
	if !w.webview.valid() {
		return
	}
	p, _ := syscall.UTF16PtrFromString(url)
	w.webview.call(5, uintptr(unsafe.Pointer(p))) // Navigate
}

// showUI must run on the tray (UI) thread.
func showUI(url string) {
	ui.pending = url
	switch ui.state {
	case wvStateFailed:
		openAppWindow(url)
		return
	case wvStateNone:
		if !ui.create() {
			ui.state = wvStateFailed
			openAppWindow(url)
			return
		}
		ui.state = wvStateCreating
		ui.startWebView()
		if ui.state == wvStateFailed {
			return
		}
	case wvStateReady:
		ui.navigate(url)
	}
	if ui.hwnd != 0 {
		if r, _, _ := procIsIconic.Call(ui.hwnd); r != 0 {
			procShowWindow.Call(ui.hwnd, swRestore)
		} else {
			procShowWindow.Call(ui.hwnd, swShow)
		}
		procBringWindowToTop.Call(ui.hwnd)
		procSetForegroundWindow.Call(ui.hwnd)
	}
}

// openAppWindow opens Edge in app mode (own window, no browser UI), or the default browser.
func openAppWindow(url string) {
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		edge := filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, err := os.Stat(edge); err == nil {
			cmd := exec.Command(edge, "--app="+url, "--window-size=1180,880",
				"--user-data-dir="+filepath.Join(dataDir(), "EdgeApp"), "--no-first-run")
			if err := cmd.Start(); err == nil {
				return
			}
		}
	}
	shellOpen(url, "")
}

// signalRunningInstance asks an already-running Deej Mixer to show its window.
func signalRunningInstance() bool {
	cls, _ := syscall.UTF16PtrFromString("DeejMixerTrayWindow")
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	if h == 0 {
		return false
	}
	procAllowSetFgWindow.Call(^uintptr(0)) // ASFW_ANY: let it take focus
	procPostMessageW.Call(h, wmOpenUI, 0, 0)
	return true
}

// makeAppLike removes everything that gives away the web engine: right-click menu, dev tools, zoom,
// status bar, browser shortcuts (F5, Ctrl+P, Ctrl+F…), and the white flash while loading.
func (w *uiWindow) makeAppLike() {
	var sp unsafe.Pointer
	if hr := w.webview.call(3, uintptr(unsafe.Pointer(&sp))); !failed(hr) && sp != nil { // get_Settings
		st := com{sp}
		st.call(10, 0) // put_IsStatusBarEnabled(FALSE)
		st.call(12, 0) // put_AreDevToolsEnabled(FALSE)
		st.call(14, 0) // put_AreDefaultContextMenusEnabled(FALSE)
		st.call(18, 0) // put_IsZoomControlEnabled(FALSE)
		if s3, err := st.query(iidWebView2Settings3); err == nil {
			s3.call(24, 0) // put_AreBrowserAcceleratorKeysEnabled(FALSE)
			s3.release()
		}
		st.release()
	}
	if c2, err := w.controller.query(iidWebView2Controller2); err == nil {
		// COREWEBVIEW2_COLOR {A,R,G,B} = opaque #0B0D12
		c2.call(27, uintptr(0xFF)|uintptr(0x0B)<<8|uintptr(0x0D)<<16|uintptr(0x12)<<24) // put_DefaultBackgroundColor
		c2.release()
	}
}
