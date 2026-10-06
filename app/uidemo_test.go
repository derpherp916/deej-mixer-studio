//go:build uidemo

package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// Serves the real UI backed by a simulated mixer, for browser checks. Runs until UIDEMO_SECONDS elapse.
func TestUIDemo(t *testing.T) {
	audio := &fakeAudio{}
	audio.snap.Master, audio.snap.Mic = &fakeVol{}, &fakeVol{}
	for _, n := range []string{"opera.exe", "discord.exe", "spotify.exe", "chrome.exe"} {
		audio.add(n, `c:\apps\`+n, &fakeVol{})
	}
	audio.add("eldenring.exe", `d:\steam\steamapps\common\ELDEN RING\game\eldenring.exe`, &fakeVol{})
	audio.snap.Procs = []ProcInfo{{PID: 3, Name: "eldenring.exe", Path: `d:\steam\steamapps\common\elden ring\game\eldenring.exe`, Created: 9}}
	port := newFakePort()
	port.hello = true
	plat := &fakePlat{audio: audio, ports: map[string]*fakePort{"COM5": port}}
	dir := t.TempDir()
	var srv *Server
	eng := NewEngine(plat, DefaultConfig(), func(s Snapshot) { srv.Publish(s) })
	studio := NewStudio(dir, fakeTools{}, func(hex string, done func(error)) error {
		go func() { time.Sleep(time.Second); done(nil) }()
		return nil
	})
	srv = NewServer(eng, defaultStore(), dir, Hooks{StartupEnabled: func() bool { return true }, IsInstalled: func() bool { return false }}, studio)
	gh := newFakeGitHub(t, "v2.1.0", nil)
	withUpdateConfig(t, gh.srv.URL, "me/deej-mixer-studio", "")
	up := NewUpdater(dir, nil, func(string) error { return nil })
	up.Check(false)
	srv.SetUpdater(up)
	l, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	stop := make(chan struct{})
	go eng.Run(stop)
	os.WriteFile("/tmp/uidemo.url", []byte(srv.URL()), 0o644)
	fmt.Println("URL", srv.URL())
	go func() {
		v := [5]int{800, 512, 300, 0, 1023}
		for i := 0; ; i++ {
			mask := 0
			if os.Getenv("UIDEMO_QUIET") == "" && i%50 < 10 {
				mask = 1 << (uint(i/50) % 6)
				if i%50 == 0 {
					port.in <- []byte(fmt.Sprintf("P,%d\r\n", i/50%6))
				}
				if i%50 == 9 {
					port.in <- []byte(fmt.Sprintf("R,%d\r\n", i/50%6))
				}
			}
			if os.Getenv("UIDEMO_QUIET") == "" {
				v[1] = (i * 20) % 1024
			} else {
				v = [5]int{720, 640, 420, 300, 880}
			}
			port.frame(v, mask)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(60 * time.Second)
	close(stop)
}
