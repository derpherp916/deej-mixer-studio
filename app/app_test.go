package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- fakes ----------

type fakeVol struct {
	mu    sync.Mutex
	vol   float32
	muted bool
	sets  int
}

func (f *fakeVol) SetVolume(v float32) error {
	f.mu.Lock()
	f.vol = v
	f.sets++
	f.mu.Unlock()
	return nil
}
func (f *fakeVol) Muted() (bool, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.muted, nil }
func (f *fakeVol) SetMute(m bool) error { f.mu.Lock(); f.muted = m; f.mu.Unlock(); return nil }
func (f *fakeVol) get() float32         { f.mu.Lock(); defer f.mu.Unlock(); return f.vol }
func (f *fakeVol) count() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.sets }

type fakeAudio struct {
	mu   sync.Mutex
	snap AudioSnapshot
}

func (a *fakeAudio) Refresh() (*AudioSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.snap
	s.Sessions = append([]SessionInfo(nil), a.snap.Sessions...)
	s.Ctls = append([]VolumeCtl(nil), a.snap.Ctls...)
	s.Procs = append([]ProcInfo(nil), a.snap.Procs...)
	return &s, nil
}
func (a *fakeAudio) Close() {}
func (a *fakeAudio) add(name, path string, v *fakeVol) {
	a.mu.Lock()
	a.snap.Sessions = append(a.snap.Sessions, SessionInfo{Key: name + path, Name: name, Path: path})
	a.snap.Ctls = append(a.snap.Ctls, v)
	a.mu.Unlock()
}

// fakePort is the PC side of a simulated Nano.
type fakePort struct {
	in      chan []byte
	mu      sync.Mutex
	written []string
	closed  bool
	hello   bool
}

func newFakePort() *fakePort { return &fakePort{in: make(chan []byte, 1000)} }
func (p *fakePort) Read(b []byte) (int, error) {
	d, ok := <-p.in
	if !ok {
		return 0, errors.New("closed")
	}
	return copy(b, d), nil
}
func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		p.written = append(p.written, l)
		if l == "?" && p.hello {
			p.in <- []byte(helloLine + "\r\n")
		}
	}
	return len(b), nil
}
func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.in)
	}
	return nil
}
func (p *fakePort) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.written...)
}
func (p *fakePort) frame(v [5]int, mask int) {
	p.in <- []byte(fmt.Sprintf("V,%d,%d,%d,%d,%d,%d\r\n", v[0], v[1], v[2], v[3], v[4], mask))
}

type fakePlat struct {
	audio    *fakeAudio
	ports    map[string]*fakePort
	mu       sync.Mutex
	keys     [][]uint16
	launched []string
}

func (f *fakePlat) ThreadInit() func()              { return func() {} }
func (f *fakePlat) NewAudio() (AudioBackend, error) { return f.audio, nil }
func (f *fakePlat) ListPorts() []PortInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PortInfo
	for k := range f.ports {
		out = append(out, PortInfo{Name: k, Desc: "USB-SERIAL CH340 (" + k + ")"})
	}
	return out
}
func (f *fakePlat) OpenPort(n string) (Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.ports[n]; ok {
		return p, nil
	}
	return nil, errors.New("no port")
}
func (f *fakePlat) SendKeys(v []uint16) error {
	f.mu.Lock()
	f.keys = append(f.keys, v)
	f.mu.Unlock()
	return nil
}
func (f *fakePlat) Launch(t string) error {
	f.mu.Lock()
	f.launched = append(f.launched, t)
	f.mu.Unlock()
	return nil
}
func (f *fakePlat) UploadFirmware(port, hexPath string, logf func(string)) error {
	logf("ok")
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	plat *fakePlat
	port *fakePort
	eng  *Engine
	stop chan struct{}
	mu   sync.Mutex
	last Snapshot
	vols map[string]*fakeVol
}

func newRig(t *testing.T) *rig {
	r := &rig{vols: map[string]*fakeVol{}}
	audio := &fakeAudio{}
	r.vols["master"], r.vols["mic"] = &fakeVol{}, &fakeVol{}
	audio.snap.Master, audio.snap.Mic = r.vols["master"], r.vols["mic"]
	for _, n := range []string{"opera.exe", "discord.exe", "spotify.exe"} {
		r.vols[n] = &fakeVol{}
		audio.add(n, `c:\apps\`+n, r.vols[n])
	}
	r.vols["game"] = &fakeVol{}
	audio.add("eldenring.exe", `d:\steam\steamapps\common\elden ring\game\eldenring.exe`, r.vols["game"])
	audio.snap.Procs = []ProcInfo{{PID: 10, Name: "eldenring.exe", Path: `d:\steam\steamapps\common\elden ring\game\eldenring.exe`, Created: 5}}
	r.port = newFakePort()
	r.port.hello = true
	r.plat = &fakePlat{audio: audio, ports: map[string]*fakePort{"COM5": r.port}}
	r.eng = NewEngine(r.plat, DefaultConfig(), func(s Snapshot) { r.mu.Lock(); r.last = s; r.mu.Unlock() })
	r.stop = make(chan struct{})
	go r.eng.Run(r.stop)
	t.Cleanup(func() { close(r.stop) })
	waitFor(t, "connection", func() bool { return r.snap().Connected })
	return r
}

func (r *rig) snap() Snapshot { r.mu.Lock(); defer r.mu.Unlock(); return r.last }

// ---------- tests ----------

func TestParseLine(t *testing.T) {
	m := ParseLine("V,0,256,512,768,1023,63\r")
	if m.Kind != MsgFrame || m.Values != [5]int{0, 256, 512, 768, 1023} || m.Mask != 63 {
		t.Fatalf("frame parse: %+v", m)
	}
	for _, bad := range []string{"V,0,0,0,0,0,64", "V,-1,0,0,0,0,0", "V,0,0,0,0,0", "V,a,0,0,0,0,0", "P,6", "R,-1", "junk", ""} {
		if ParseLine(bad).Kind != MsgNone {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if m := ParseLine("P,3"); m.Kind != MsgPress || m.Button != 3 {
		t.Error("press")
	}
	if ParseLine(helloLine+"\r").Kind != MsgHello {
		t.Error("hello")
	}
}

func TestCommandChecksum(t *testing.T) {
	got := string(Command("T,1"))
	// 'T'^','^'1' = 0x54^0x2C^0x31 = 0x49
	if got != "T,1*49\n" {
		t.Fatalf("got %q", got)
	}
}

func TestLineSplitter(t *testing.T) {
	var s LineSplitter
	var got []string
	s.Feed([]byte("HEL"), func(l string) { got = append(got, l) })
	s.Feed([]byte("LO\r\nP,1\r\n"+strings.Repeat("x", 1000)+"\n"), func(l string) { got = append(got, l) })
	if len(got) != 3 || got[0] != "HELLO" || got[1] != "P,1" || len(got[2]) != 256 {
		t.Fatalf("%q", got)
	}
}

func TestHotkeys(t *testing.T) {
	v, err := ParseHotkey("ctrl + shift + m")
	if err != nil || len(v) != 3 || v[0] != 0x11 || v[2] != 'M' {
		t.Fatal(v, err)
	}
	for _, bad := range []string{"", "CTRL+", "CTRL+CTRL", "HYPER+X"} {
		if _, err := ParseHotkey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConfigNormalize(t *testing.T) {
	c := DefaultConfig()
	n, err := c.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if n.Pots[2].Target != TargetSteamGame {
		t.Fatal(n.Pots[2].Target)
	}
	c.Pots[1].Target = " Opera.EXE ,  Chrome.exe "
	c.Pots[2].Target = "steam_recent"
	c.Buttons[0] = ButtonConfig{Action: ActHotkey, Value: "ctrl + f13"}
	c.Port = "com7"
	n, err = c.Normalize()
	if err != nil || n.Pots[1].Target != "opera.exe, chrome.exe" || n.Pots[2].Target != TargetSteamGame || n.Buttons[0].Value != "CTRL+F13" || n.Port != "COM7" {
		t.Fatalf("%+v %v", n, err)
	}
	bad := []func(*Config){
		func(c *Config) { c.Pots[0].Target = " " },
		func(c *Config) { c.Buttons[1] = ButtonConfig{Action: ActMute, Value: "5"} },
		func(c *Config) { c.Buttons[1] = ButtonConfig{Action: ActLaunch} },
		func(c *Config) { c.Buttons[1] = ButtonConfig{Action: "explode"} },
		func(c *Config) { c.LogoColors[0] = "red" },
		func(c *Config) { c.Brightness = 101 },
		func(c *Config) { c.LogoChannel[0] = 7 },
		func(c *Config) { c.Port = "/dev/tty" },
	}
	for i, f := range bad {
		c := DefaultConfig()
		f(&c)
		if _, err := c.Normalize(); err == nil {
			t.Errorf("bad config %d accepted", i)
		}
	}
}

func TestConfigSaveLoad(t *testing.T) {
	dir := t.TempDir()
	c, _ := DefaultConfig().Normalize()
	c.Pots[3].Label = "Comms"
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(dir)
	if err != nil || got.Pots[3].Label != "Comms" {
		t.Fatal(got, err)
	}
	// corrupt file falls back to defaults with an error
	SaveConfig(dir, c)
	writeFile(t, dir+"/config.json", "{not json")
	got, err = LoadConfig(dir)
	if err == nil || got.Pots[3].Label != "Discord" {
		t.Fatal("corrupt config should fall back to defaults")
	}
}

func TestNewestGame(t *testing.T) {
	procs := []ProcInfo{
		{Name: "steam.exe", Path: `c:\steam\steam.exe`, Created: 1},
		{Name: "steamwebhelper.exe", Path: `c:\steam\bin\steamwebhelper.exe`, Created: 99},
		{Name: "hl2.exe", Path: `c:\steam\steamapps\common\half-life 2\hl2.exe`, Created: 10},
		{Name: "eldenring.exe", Path: `d:\lib\steamapps\common\elden ring\game\eldenring.exe`, Created: 20},
		// helper of the OLDER game launched after the newer game: must not win
		{Name: "crashpad.exe", Path: `c:\steam\steamapps\common\half-life 2\bin\crashpad.exe`, Created: 30},
		{Name: "unitycrashhandler64.exe", Path: `d:\lib\steamapps\common\x\unitycrashhandler64.exe`, Created: 50},
	}
	g := NewestGame(procs, nil)
	if g == nil || g.Title != "elden ring" {
		t.Fatalf("%+v", g)
	}
	g = NewestGame(append(procs, ProcInfo{Name: "minecraft.exe", Path: `c:\mc\minecraft.exe`, Created: 40}), []string{"Minecraft.exe"})
	if g == nil || g.Title != "minecraft" || !g.Exes["minecraft.exe"] {
		t.Fatalf("%+v", g)
	}
	if NewestGame(procs[:2], nil) != nil {
		t.Fatal("no game expected")
	}
}

func TestSessionsFor(t *testing.T) {
	sessions := []SessionInfo{
		{Name: "opera.exe", Path: `c:\opera\opera.exe`},
		{Name: "steam.exe", Path: `c:\steam\steam.exe`},
		{Name: "eldenring.exe", Path: `d:\lib\steamapps\common\elden ring\game\eldenring.exe`},
		{Name: "chrome.exe", Path: `c:\chrome\chrome.exe`},
		{System: true},
	}
	game := &Game{Title: "elden ring", Dir: `d:\lib\steamapps\common\elden ring\`}
	targets := []string{"master", "opera", "steam_game", "system", "other"}
	expect := [][]int{nil, {0}, {2}, {4}, {1, 3}}
	for i := range targets {
		got := SessionsFor(i, targets, sessions, game)
		if fmt.Sprint(got) != fmt.Sprint(expect[i]) {
			t.Errorf("pot %d (%s): got %v want %v", i, targets[i], got, expect[i])
		}
	}
	// No game running: steam_game controls the Steam client.
	if got := SessionsFor(2, targets, sessions, nil); fmt.Sprint(got) != "[1]" {
		t.Errorf("fallback to steam client: %v", got)
	}
}

func TestEngineVolumesMuteAndLEDs(t *testing.T) {
	r := newRig(t)
	r.port.frame([5]int{1023, 512, 256, 0, 1023}, 0)
	waitFor(t, "volumes", func() bool { return r.vols["master"].get() == 1 })
	if v := r.vols["opera.exe"].get(); v < 0.49 || v > 0.51 {
		t.Errorf("opera %v", v)
	}
	if v := r.vols["game"].get(); v < 0.24 || v > 0.26 {
		t.Errorf("steam game should get A2: %v", v)
	}
	if r.vols["discord.exe"].get() != 0 || r.vols["spotify.exe"].get() != 1 {
		t.Errorf("discord/spotify %v %v", r.vols["discord.exe"].get(), r.vols["spotify.exe"].get())
	}
	// Changing volume elsewhere is respected until the knob moves.
	r.vols["opera.exe"].SetVolume(0.9)
	r.port.frame([5]int{1023, 513, 256, 0, 1023}, 0)
	time.Sleep(80 * time.Millisecond)
	if r.vols["opera.exe"].get() != 0.9 {
		t.Error("1-step jitter must not override an external change")
	}
	r.port.frame([5]int{1023, 600, 256, 0, 1023}, 0)
	waitFor(t, "opera follows knob", func() bool { v := r.vols["opera.exe"].get(); return v > 0.58 && v < 0.59 })

	// LEDs: ring full, logos on (sessions active)
	waitFor(t, "LED frame", func() bool { return containsPrefix(r.port.sent(), "L,7,2,2,2,2,") })
	// Opera button: press -> mute immediately (no hold action)
	r.port.in <- []byte("P,0\r\n")
	waitFor(t, "opera muted", func() bool { m, _ := r.vols["opera.exe"].Muted(); return m })
	r.port.in <- []byte("R,0\r\n")
	waitFor(t, "logo 1 off", func() bool { return containsPrefix(r.port.sent(), "L,7,0,2,2,2,") })
	// press again -> unmute
	r.port.in <- []byte("P,0\r\nR,0\r\n")
	waitFor(t, "opera unmuted", func() bool { m, _ := r.vols["opera.exe"].Muted(); return !m })

	// Small left (D6) mutes master -> everything dark
	r.port.in <- []byte("P,4\r\nR,4\r\n")
	waitFor(t, "master muted", func() bool { m, _ := r.vols["master"].Muted(); return m })
	waitFor(t, "all dark", func() bool { return containsPrefix(r.port.sent(), "L,0,0,0,0,0,") })
	r.port.in <- []byte("P,4\r\nR,4\r\n")
	// Small right (D7) mutes mic -> ring colour changes to mic colour
	r.port.in <- []byte("P,5\r\nR,5\r\n")
	waitFor(t, "mic muted", func() bool { m, _ := r.vols["mic"].Muted(); return m })
	waitFor(t, "ring mic colour", func() bool { return containsPrefix(r.port.sent(), "C,4,FF2000") })

	// Every command sent carries a valid checksum.
	for _, l := range r.port.sent() {
		if l == "?" {
			continue
		}
		i := strings.LastIndexByte(l, '*')
		if i < 0 || string(Command(l[:i])) != l+"\n" {
			t.Fatalf("bad command %q", l)
		}
	}
}

func TestEngineHoldAndNewSession(t *testing.T) {
	r := newRig(t)
	r.port.frame([5]int{1023, 512, 256, 0, 1023}, 0)
	// Steam button has a hold action (launch steam://open/main): short press mutes on release.
	r.port.in <- []byte("P,1\r\n")
	time.Sleep(100 * time.Millisecond)
	if m, _ := r.vols["game"].Muted(); m {
		t.Fatal("short action must wait for release when a hold action exists")
	}
	r.port.in <- []byte("R,1\r\n")
	waitFor(t, "game muted", func() bool { m, _ := r.vols["game"].Muted(); return m })
	// Long hold launches and does not toggle mute.
	r.port.in <- []byte("P,1\r\n")
	waitFor(t, "launch", func() bool { r.plat.mu.Lock(); defer r.plat.mu.Unlock(); return len(r.plat.launched) == 1 })
	r.port.in <- []byte("R,1\r\n")
	time.Sleep(80 * time.Millisecond)
	if m, _ := r.vols["game"].Muted(); !m {
		t.Fatal("hold must not also toggle mute")
	}
	// Mute Discord while it is not running, then it starts: it starts muted at the knob's level.
	audio := r.plat.audio
	audio.mu.Lock()
	audio.snap.Sessions = audio.snap.Sessions[:0]
	audio.snap.Ctls = audio.snap.Ctls[:0]
	audio.mu.Unlock()
	r.eng.Send(engineCmd{kind: "config", cfg: DefaultConfig()}) // forces remap
	r.port.in <- []byte("P,2\r\nR,2\r\n")
	time.Sleep(100 * time.Millisecond)
	nv := &fakeVol{vol: 1}
	audio.add("discord.exe", `c:\discord\discord.exe`, nv)
	waitFor(t, "new discord session muted", func() bool { m, _ := nv.Muted(); return m && nv.get() == 0 })
}

func TestEngineHardwareTest(t *testing.T) {
	r := newRig(t)
	if err := r.eng.Send(engineCmd{kind: "test_start"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "T,1", func() bool { return containsPrefix(r.port.sent(), "T,1*") })
	opera := r.vols["opera.exe"].count()
	for i := 0; i < 6; i++ {
		r.port.in <- []byte(fmt.Sprintf("P,%d\r\nR,%d\r\n", i, i))
	}
	r.port.frame([5]int{0, 0, 0, 0, 0}, 0)
	r.port.frame([5]int{1023, 1023, 1023, 1023, 1023}, 0)
	waitFor(t, "test results", func() bool {
		s := r.snap().Test
		return s.Releases == [6]int{1, 1, 1, 1, 1, 1} && s.Max[4] == 1023 && s.Min[0] == 0
	})
	if r.vols["opera.exe"].count() != opera {
		t.Error("volumes must be paused in test mode")
	}
	if m, _ := r.vols["opera.exe"].Muted(); m {
		t.Error("buttons must not act in test mode")
	}
	r.eng.Send(engineCmd{kind: "pattern", n: 4})
	waitFor(t, "chase", func() bool { return containsPrefix(r.port.sent(), "T,4*") })
	r.eng.Send(engineCmd{kind: "test_stop"})
	waitFor(t, "T,0", func() bool { return containsPrefix(r.port.sent(), "T,0*") })
}

func TestEngineReconnects(t *testing.T) {
	r := newRig(t)
	r.port.Close()
	waitFor(t, "disconnect", func() bool { return !r.snap().Connected })
	p2 := newFakePort()
	p2.hello = true
	r.plat.mu.Lock()
	r.plat.ports["COM5"] = p2
	r.plat.mu.Unlock()
	go func() {
		for i := 0; i < 400; i++ {
			p2.mu.Lock()
			closed := p2.closed
			p2.mu.Unlock()
			if closed {
				return
			}
			p2.frame([5]int{1, 2, 3, 4, 5}, 0)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for i := 0; i < 600 && !r.snap().Connected; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !r.snap().Connected {
		t.Fatal("did not reconnect")
	}
}

func TestWebSecurityAndConfig(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	s := NewServer(r.eng, defaultStore(), dir, Hooks{}, NewStudio(dir, nil, nil))
	s.addr = "127.0.0.1:47957"
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/api/config", s.api(s.postConfig))
	mux.HandleFunc("/api/state", s.api(s.getState))
	h := s.guard(mux)
	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1:47957"+path, strings.NewReader(body))
		req.Host = "127.0.0.1:47957"
		for k, v := range hdr {
			if k == "Host" {
				req.Host = v
			} else {
				req.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	tok := map[string]string{"X-Deej-Token": s.token, "Content-Type": "application/json"}
	if w := do("GET", "/api/state", "", nil); w.Code != 403 {
		t.Fatal("state without token must be refused", w.Code)
	}
	if w := do("GET", "/api/state", "", map[string]string{"X-Deej-Token": s.token, "Host": "evil.example:47957"}); w.Code != 403 {
		t.Fatal("foreign Host must be refused (DNS rebinding)")
	}
	if w := do("POST", "/api/config", "{}", map[string]string{"X-Deej-Token": s.token, "Content-Type": "application/json", "Origin": "http://evil.example"}); w.Code != 403 {
		t.Fatal("cross-origin POST must be refused")
	}
	if w := do("GET", "/?t="+s.token, "", nil); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("Firmware Studio")) {
		t.Fatal("page with token", w.Code)
	}
	c := DefaultConfig()
	c.Pots[1].Target = "chrome.exe"
	body, _ := json.Marshal(c)
	if w := do("POST", "/api/config", string(body), tok); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	ps, _ := LoadProfiles(dir)
	got := ps.Current()
	if got.Pots[1].Target != "chrome.exe" {
		t.Fatal("config not saved")
	}
	c.Buttons[0].Action = "explode"
	body, _ = json.Marshal(c)
	if w := do("POST", "/api/config", string(body), tok); w.Code != 400 || !strings.Contains(w.Body.String(), "unknown action") {
		t.Fatal("invalid config must be rejected", w.Body.String())
	}
}

func TestIcon(t *testing.T) {
	img := drawIcon(32)
	if img.RGBAAt(16, 16).A != 255 || img.RGBAAt(0, 0).A != 0 {
		t.Fatal("icon centre should be opaque and corner transparent")
	}
}

func containsPrefix(lines []string, p string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeReportsWhy(t *testing.T) {
	audio := &fakeAudio{}
	silent := newFakePort() // opens but never answers
	plat := &badPlat{fakePlat: fakePlat{audio: audio, ports: map[string]*fakePort{"COM3": silent}}}
	var mu sync.Mutex
	var last Snapshot
	eng := NewEngine(plat, DefaultConfig(), func(s Snapshot) { mu.Lock(); last = s; mu.Unlock() })
	stop := make(chan struct{})
	defer close(stop)
	go eng.Run(stop)
	for i := 0; i < 800; i++ {
		mu.Lock()
		r := last.ProbeReport
		mu.Unlock()
		if len(r) == 2 {
			j := strings.Join(r, "\n")
			if !strings.Contains(j, "COM5: could not open: in use by another program") || !strings.Contains(j, "COM3: opened, but no data arrived") {
				t.Fatalf("report: %s", j)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no probe report")
}

type badPlat struct{ fakePlat }

func (b *badPlat) ListPorts() []PortInfo {
	return []PortInfo{{Name: "COM3", Desc: "Communications Port (COM3)"}, {Name: "COM5", Desc: "USB-SERIAL CH340 (COM5)"}, {Name: "COM9", Desc: "Standard Serial over Bluetooth link (COM9)"}}
}
func (b *badPlat) OpenPort(n string) (Port, error) {
	if n == "COM5" {
		return nil, errors.New("Access is denied.")
	}
	if n == "COM9" {
		panic("bluetooth port must never be probed")
	}
	return b.fakePlat.OpenPort(n)
}

func TestOldFirmwareDetectedAndBrightness(t *testing.T) {
	p := newFakePort()
	go func() {
		for i := 0; i < 50; i++ {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return
			}
			p.in <- []byte("DEEJCTRL,1\r\nD,1,2,3,4,5,0\r\n")
			time.Sleep(100 * time.Millisecond)
		}
	}()
	_, why := probePort(&fakePlat{ports: map[string]*fakePort{"COM5": p}}, "COM5")
	if !strings.Contains(why, "old ChatGPT DeejControl firmware") {
		t.Fatalf("got %q", why)
	}
	p2 := newFakePort()
	go func() { time.Sleep(200 * time.Millisecond); p2.in <- []byte("HELLO,DEEJMIXER,2\r\n") }()
	_, why = probePort(&fakePlat{ports: map[string]*fakePort{"COM6": p2}}, "COM6")
	if !strings.Contains(why, "older Deej Mixer firmware (v2)") {
		t.Fatalf("got %q", why)
	}
	e := NewEngine(&fakePlat{}, DefaultConfig(), func(Snapshot) {})
	body, _ := e.ledState()
	if !strings.HasSuffix(body, ",96,255") { // ring 60% of 160, logos 100% of 255
		t.Fatalf("LED command %q", body)
	}
}

// fakeTools simulates arduino-cli for Studio tests.
type fakeTools struct{ failCompile bool }

func (fakeTools) FindCLI(logf func(string)) (string, error) { return "arduino-cli", nil }
func (f fakeTools) Run(exe string, args []string, logf func(string)) (string, error) {
	out := ""
	switch args[0] {
	case "core":
		out = "ID          Installed\narduino:avr 1.8.6\n"
	case "lib":
		out = "Adafruit NeoPixel 1.15.5\n"
	case "compile":
		if f.failCompile {
			out = "DeejMixer.ino:12:3: error: 'foo' was not declared in this scope\n"
			if logf != nil {
				logf(out)
			}
			return out, errors.New("exit status 1")
		}
		dir := ""
		for i, a := range args {
			if a == "--output-dir" {
				dir = args[i+1]
			}
		}
		os.MkdirAll(dir, 0o755)
		os.WriteFile(dir+"/DeejMixer.ino.hex", []byte(":00000001FF\n"), 0o644)
		out = "Sketch uses 7728 bytes (25%) of program storage space.\n"
	}
	if logf != nil {
		logf(out)
	}
	return out, nil
}

func waitJob(t *testing.T, s *Studio) StudioJob {
	for i := 0; i < 300; i++ {
		if j := s.Job(); !j.Running {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return StudioJob{}
}

func TestStudio(t *testing.T) {
	dir := t.TempDir()
	uploaded := ""
	s := NewStudio(dir, fakeTools{}, func(hex string, done func(error)) error {
		uploaded = hex
		go done(nil)
		return nil
	})
	src, modified, err := s.Load()
	if err != nil || modified || !strings.Contains(src, "Deej Mixer firmware") {
		t.Fatalf("load: %v %v", modified, err)
	}
	if err := s.Save(src + "\n// my change\n"); err != nil {
		t.Fatal(err)
	}
	if _, modified, _ = s.Load(); !modified {
		t.Fatal("edit should mark the sketch modified")
	}
	if _, err := s.Start(true); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, s)
	if !j.OK || !strings.Contains(uploaded, "DeejMixer.ino.hex") || !strings.Contains(j.Log, "Upload verified") {
		t.Fatalf("upload job: %+v uploaded=%q", j, uploaded)
	}
	s.tools = fakeTools{failCompile: true}
	s.Start(false)
	j = waitJob(t, s)
	if j.OK || !strings.Contains(j.Log, "was not declared") {
		t.Fatalf("failing compile: %+v", j)
	}
	s.Reset()
	if _, modified, _ = s.Load(); modified {
		t.Fatal("reset should restore stock firmware")
	}
}

func TestProfiles(t *testing.T) {
	dir := t.TempDir()
	c := DefaultConfig()
	c.Pots[1].Label = "Browser"
	c.Port = "COM5"
	c, _ = c.Normalize()
	SaveConfig(dir, c) // old single-config file is migrated
	ps, err := LoadProfiles(dir)
	if err != nil || ps.Active != "Default" || ps.Current().Pots[1].Label != "Browser" {
		t.Fatalf("migration: %v %+v", err, ps)
	}
	if _, err := ps.Apply("duplicate", "", "Gaming"); err != nil {
		t.Fatal(err)
	}
	g := ps.Current()
	g.Pots[1].Label = "Chrome"
	ps.SetCurrent(g)
	if _, err := ps.Apply("create", "", "Gaming"); err == nil {
		t.Fatal("duplicate names must be refused")
	}
	cfg, _ := ps.Apply("switch", "Default", "")
	if cfg.Pots[1].Label != "Browser" || cfg.Port != "COM5" {
		t.Fatalf("switch: %+v", cfg.Pots[1])
	}
	ps.Apply("rename", "Gaming", "Games")
	ps.Save(dir)
	ps2, _ := LoadProfiles(dir)
	if len(ps2.Order) != 2 || ps2.Order[1] != "Games" || ps2.Profiles["Games"].Pots[1].Label != "Chrome" {
		t.Fatalf("reload: %+v", ps2.Order)
	}
	ps2.Apply("delete", "Games", "")
	if _, err := ps2.Apply("delete", "Default", ""); err == nil {
		t.Fatal("last profile must not be deletable")
	}
}

func TestMonitor(t *testing.T) {
	r := newRig(t)
	mon := newFakePort() // a fresh handle for the monitor (closing the mixer's handle ends that fake)
	r.plat.mu.Lock()
	r.plat.ports["COM5"] = mon
	r.plat.mu.Unlock()
	if err := r.eng.Send(engineCmd{kind: "monitor_start", text: "COM5"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "monitor", func() bool { return r.snap().Monitor && !r.snap().Connected })
	mon.in <- []byte("hello from sketch\r\nline2\r\n")
	var lines []string
	waitFor(t, "lines", func() bool { lines, _ = r.eng.MonitorSince(0); return len(lines) >= 2 })
	if lines[0] != "hello from sketch" {
		t.Fatalf("%q", lines)
	}
	r.eng.Send(engineCmd{kind: "monitor_send", text: "?"})
	waitFor(t, "sent", func() bool { return containsPrefix(mon.sent(), "?") })
	_, seq := r.eng.MonitorSince(0)
	if more, _ := r.eng.MonitorSince(seq); len(more) != 0 {
		t.Fatal("no new lines expected")
	}
	r.eng.Send(engineCmd{kind: "monitor_stop"})
	waitFor(t, "not monitoring", func() bool { return !r.snap().Monitor })
}
