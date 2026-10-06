//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func init() { runtime.LockOSThread() } // the tray's message loop must own the main thread

func hasArg(a string) bool {
	for _, x := range os.Args[1:] {
		if strings.EqualFold(x, a) {
			return true
		}
	}
	return false
}

func argAfter(a string) string {
	for i, x := range os.Args[1:] {
		if strings.EqualFold(x, a) && i+2 < len(os.Args) {
			return os.Args[i+2]
		}
	}
	return ""
}

func setupLog(dir string) {
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "deejmixer.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
		os.Rename(p, p+".old")
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(log.LstdFlags)
}

type runtimeInfo struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

func main() {
	dir := dataDir()
	setupLog(dir)
	if pid, err := strconv.Atoi(argAfter("--after-install")); err == nil {
		waitForPID(pid, 10*time.Second)
	}
	if hasArg("--quit") { // used by the installer/uninstaller to close a running copy cleanly
		cls, _ := syscall.UTF16PtrFromString("DeejMixerTrayWindow")
		if h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0); h != 0 {
			procPostMessageW.Call(h, wmQuit, 0, 0)
			for i := 0; i < 50 && !singleInstanceFree(); i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
		return
	}
	if hasArg("--selftest") {
		selfTest(dir)
		return
	}
	if !singleInstance() {
		// Already running: bring its window up.
		if !signalRunningInstance() {
			messageBox("Deej Mixer is already running. Use its icon in the taskbar notification area.", "Deej Mixer", 0x40)
		}
		return
	}
	log.Printf("Deej Mixer %s starting from %s", AppVersion, exeDir())
	profiles, err := LoadProfiles(dir)
	if err != nil {
		log.Print(err)
	}
	cfg := profiles.Current()

	var srv *Server
	var eng *Engine
	lastTip := ""
	fwNotified := false
	eng = NewEngine(winPlatform{}, cfg, func(s Snapshot) {
		srv.Publish(s)
		tip := "Deej Mixer – not connected"
		if s.Connected {
			tip = "Deej Mixer – connected (" + s.Port + ")"
		}
		if !fwNotified && theTray != nil {
			for _, r := range s.ProbeReport {
				if strings.Contains(r, "Upload firmware") { // the mixer runs older firmware than this app
					fwNotified = true
					theTray.Notify("Mixer firmware update ready", "Click to update your Deej Mixer's firmware (about 10 seconds).", "settings")
				}
			}
		}
		if tip != lastTip && theTray != nil {
			lastTip = tip
			theTray.SetTip(tip)
		}
	})
	var studio *Studio
	studio = NewStudio(dir, winStudioTools{}, func(hex string, done func(error)) error {
		return eng.Send(engineCmd{kind: "upload", hex: hex, done: func(err error) {
			studio.logf("%s", eng.UploadLog())
			go func() { done(err) }()
		}})
	})
	srv = NewServer(eng, profiles, dir, Hooks{
		StartupEnabled: startupEnabled,
		SetStartup:     setStartup,
		Install:        install,
		IsInstalled:    isInstalled,
		OpenDrivers:    openDrivers,
		OpenPath:       func(p string) error { return shellOpen(p, "") },
	}, studio)
	updater := NewUpdater(dir, func(title, text string) {
		if theTray != nil {
			theTray.Notify(title, text, "settings")
		}
	}, func(setup string) error {
		// Silent update: the installer closes this app, replaces it (settings are kept) and restarts it.
		return shellExec("runas", setup, "/S")
	})
	srv.SetUpdater(updater)
	l, err := srv.Listen()
	if err != nil {
		messageBox("Deej Mixer could not start its control panel: "+err.Error(), "Deej Mixer", 0x10)
		return
	}
	ri, _ := json.Marshal(runtimeInfo{URL: srv.URL(), PID: os.Getpid()})
	os.WriteFile(filepath.Join(dir, "runtime.json"), ri, 0o600)
	go func() {
		if err := srv.Serve(l); err != nil {
			log.Printf("web server stopped: %v", err)
		}
	}()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { eng.Run(stop); close(done) }()
	go updater.Run(stop)

	open := func(page string) { // runs on the tray thread
		u := srv.URL()
		if page != "" {
			u += "#" + page
		}
		showUI(u)
	}
	runTray("Deej Mixer", open, !hasArg("--background"))
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	os.Remove(filepath.Join(dir, "runtime.json"))
	log.Print("Deej Mixer stopped")
}

// selfTest checks Windows audio, processes and ports without touching any volume. It writes
// selftest.json to the settings folder and shows a summary.
func selfTest(dir string) {
	runtime.LockOSThread()
	defer comInit()()
	report := map[string]any{"version": AppVersion, "exeDir": exeDir()}
	a, err := newWinAudio()
	if err != nil {
		report["audioError"] = err.Error()
	} else {
		snap, err := a.Refresh()
		if err != nil {
			report["audioError"] = err.Error()
		}
		if snap != nil {
			report["speakers"] = snap.Master != nil
			report["microphone"] = snap.Mic != nil
			if snap.Master != nil {
				m, err := snap.Master.Muted()
				report["speakersMuted"] = m
				if err != nil {
					report["speakersMuteError"] = err.Error()
				}
			}
			var names []string
			for _, s := range snap.Sessions {
				n := s.Name
				if s.System {
					n = "system"
				}
				names = append(names, n)
			}
			report["sessions"] = names
			report["processes"] = len(snap.Procs)
			if g := NewestGame(snap.Procs, nil); g != nil {
				report["steamGame"] = g.Title
			}
		}
		a.Close()
	}
	report["ports"] = listPorts()
	_, err = os.Stat(filepath.Join(exeDir(), "tools", "avrdude.exe"))
	report["avrdudePresent"] = err == nil
	report["firmwareBytes"] = len(firmwareHex)
	report["startup"] = startupEnabled()
	data, _ := json.MarshalIndent(report, "", "  ")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "selftest.json"), data, 0o644)
	messageBox(fmt.Sprintf("Deej Mixer self-test\n\n%s", data), "Deej Mixer self-test", 0x40)
}
