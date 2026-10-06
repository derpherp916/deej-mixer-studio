//go:build sim

package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// pipePort connects the engine to the simulated Nano's UART.
type pipePort struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (p *pipePort) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipePort) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipePort) Close() error                { p.w.Close(); return nil }

// TestRealFirmwareInSimulator runs the actual DeejMixer.elf in simavr and drives it with the engine.
func TestRealFirmwareInSimulator(t *testing.T) {
	cmd := exec.Command(os.Getenv("SIM_BRIDGE"), os.Getenv("FW_ELF"))
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	port := &pipePort{r: stdout, w: stdin}
	audio := &fakeAudio{}
	vols := map[string]*fakeVol{"master": {}, "opera.exe": {}, "game": {}, "discord.exe": {}, "spotify.exe": {}}
	audio.snap.Master = vols["master"]
	for _, n := range []string{"opera.exe", "discord.exe", "spotify.exe"} {
		audio.add(n, `c:\`+n, vols[n])
	}
	audio.add("game.exe", `c:\steam\steamapps\common\game\game.exe`, vols["game"])
	audio.snap.Procs = []ProcInfo{{PID: 9, Name: "game.exe", Path: `c:\steam\steamapps\common\game\game.exe`, Created: 1}}
	plat := &simPlat{fakePlat: fakePlat{audio: audio}, port: port}
	var last Snapshot
	eng := NewEngine(plat, DefaultConfig(), func(s Snapshot) { last = s })
	stop := make(chan struct{})
	go eng.Run(stop)
	waitLong := func(what string, d time.Duration, f func() bool) {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if f() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timeout: %s (status %q connected %v values %v)", what, eng.status, eng.conn != nil, eng.values)
	}
	waitLong("volumes from real firmware", 4*time.Second, func() bool {
		return vols["master"].get() == 1 && vols["spotify.exe"].get() == 1 && vols["discord.exe"].get() == 0 &&
			vols["opera.exe"].get() > 0.49 && vols["opera.exe"].get() < 0.51 && vols["game"].get() > 0.24 && vols["game"].get() < 0.26
	})
	t.Logf("volumes: master %.3f opera %.3f game %.3f discord %.3f spotify %.3f", vols["master"].get(), vols["opera.exe"].get(),
		vols["game"].get(), vols["discord.exe"].get(), vols["spotify.exe"].get())
	waitLong("Opera button press from firmware mutes Opera", 3*time.Second, func() bool { m, _ := vols["opera.exe"].Muted(); return m })
	waitLong("second press unmutes", 3*time.Second, func() bool { m, _ := vols["opera.exe"].Muted(); return !m })
	time.Sleep(600 * time.Millisecond)
	close(stop)
	time.Sleep(200 * time.Millisecond)
	stdin.Close()
	cmd.Wait()
	t.Logf("simulator: %s", strings.TrimSpace(stderr.String()))
	if !strings.Contains(stderr.String(), "LED_EDGES") || strings.Contains(stderr.String(), "D12=0 ") {
		t.Fatal("firmware never drove the ring LEDs from engine commands")
	}
	_ = last
}

type simPlat struct {
	fakePlat
	port *pipePort
	used bool
}

func (s *simPlat) ListPorts() []PortInfo { return []PortInfo{{Name: "SIM"}} }
func (s *simPlat) OpenPort(string) (Port, error) {
	if s.used {
		return nil, io.EOF
	}
	s.used = true
	return s.port, nil
}
