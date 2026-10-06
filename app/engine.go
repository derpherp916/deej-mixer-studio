package main

import (
	"fmt"
	"log"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- platform abstraction (Windows implementation in *_windows.go, fakes in tests) ----------

type VolumeCtl interface {
	SetVolume(v float32) error
	Muted() (bool, error)
	SetMute(m bool) error
}

type AudioSnapshot struct {
	Master   VolumeCtl // nil when no default speaker
	Mic      VolumeCtl // nil when no default microphone
	Sessions []SessionInfo
	Ctls     []VolumeCtl // parallel to Sessions
	Procs    []ProcInfo
}

type AudioBackend interface {
	Refresh() (*AudioSnapshot, error) // invalidates the previous snapshot
	Close()
}

type Port interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

type Platform interface {
	ThreadInit() func()
	NewAudio() (AudioBackend, error)
	ListPorts() []PortInfo
	OpenPort(name string) (Port, error)
	SendKeys(vks []uint16) error
	Launch(target string) error
	UploadFirmware(port, hexPath string, logf func(string)) error // hexPath "" = bundled firmware
}

// ---------- live state shared with the UI ----------

type PotLive struct {
	Percent  int      `json:"percent"`
	Muted    bool     `json:"muted"`
	Active   bool     `json:"active"` // something to control exists
	Sessions []string `json:"sessions"`
}

type TestState struct {
	Active   bool   `json:"active"`
	Pattern  int    `json:"pattern"`
	Presses  [6]int `json:"presses"`
	Releases [6]int `json:"releases"`
	Min      [5]int `json:"min"`
	Max      [5]int `json:"max"`
	Frames   int    `json:"frames"`
}

type Snapshot struct {
	Connected   bool       `json:"connected"`
	Port        string     `json:"port"`
	Status      string     `json:"status"`
	Values      [5]int     `json:"values"`
	Mask        int        `json:"mask"`
	Pots        [5]PotLive `json:"pots"`
	MasterMuted bool       `json:"masterMuted"`
	MicMuted    bool       `json:"micMuted"`
	Game        string     `json:"game"`
	AudioError  string     `json:"audioError"`
	AllApps     []string   `json:"allApps"`
	Test        TestState  `json:"test"`
	Log         []string   `json:"log"`
	Uploading   bool       `json:"uploading"`
	UploadLog   string     `json:"uploadLog"`
	Monitor     bool       `json:"monitor"`
	MonitorPort string     `json:"monitorPort"`
	MonitorSeq  int        `json:"monitorSeq"` // total lines received; the UI fetches new ones by sequence
	Ports       []PortInfo `json:"ports"`
	ProbeReport []string   `json:"probeReport"`
}

// ---------- engine ----------

type engineCmd struct {
	kind  string
	cfg   Config
	n     int
	text  string
	hex   string          // upload: custom firmware file ("" = bundled)
	done  func(err error) // upload: called when finished (from the engine thread)
	reply chan error
}

type uploadReq struct {
	port, hex string
	done      func(error)
}

type conn struct {
	name  string
	port  Port
	lines chan string
	dead  chan struct{}
	once  sync.Once
}

func (c *conn) close() { c.once.Do(func() { c.port.Close() }) }

type Engine struct {
	plat    Platform
	cfg     Config
	cmds    chan engineCmd
	publish func(Snapshot)
	now     func() time.Time

	// connection
	conn        *conn
	probing     bool
	probeRes    chan probeResult
	nextProbe   time.Time
	lastFrame   time.Time
	lastGood    string
	paused      bool
	status      string
	portsCache  []PortInfo
	probeReport []string

	// audio
	audio      AudioBackend
	snap       *AudioSnapshot
	game       *Game
	potSess    [5][]int
	lastAudio  time.Time
	audioErr   string
	lastVal    [5]float64
	haveVal    [5]bool
	applied    map[string]bool
	wantMute   [5]bool
	values     [5]int
	mask       int
	haveValues bool

	// buttons
	pressedAt [6]time.Time
	held      [6]bool
	shortDone [6]bool
	holdDone  [6]bool

	// LEDs
	lastLED     string
	lastLEDSent time.Time
	lastLEDEval time.Time
	colorsSent  bool
	lastColors  [5]string

	test          TestState
	logLines      []string
	uploading     bool
	uploadMu      sync.Mutex
	uploadLog     strings.Builder
	pendingUpload *uploadReq
	uploadDone    func(error)

	// serial monitor (Firmware Studio)
	monitor  *conn
	monMu    sync.Mutex
	monLines []string
	monSeq   int
}

func NewEngine(p Platform, cfg Config, publish func(Snapshot)) *Engine {
	return &Engine{plat: p, cfg: cfg, cmds: make(chan engineCmd, 32), publish: publish, now: time.Now,
		probeRes: make(chan probeResult, 1), applied: map[string]bool{}, status: "Starting…"}
}

func (e *Engine) Send(c engineCmd) error {
	c.reply = make(chan error, 1)
	e.cmds <- c
	select {
	case err := <-c.reply:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("engine busy")
	}
}

func (e *Engine) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Print(msg)
	e.logLines = append(e.logLines, time.Now().Format("15:04:05 ")+msg)
	if len(e.logLines) > 40 {
		e.logLines = e.logLines[len(e.logLines)-40:]
	}
}

// Run owns COM and the serial port; everything happens on this one OS thread.
func (e *Engine) Run(stop <-chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cleanup := e.plat.ThreadInit()
	defer cleanup()
	var err error
	if e.audio, err = e.plat.NewAudio(); err != nil {
		e.audioErr = "Windows audio unavailable: " + err.Error()
		e.logf("%s", e.audioErr)
	}
	e.refreshAudio()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	lastPub := time.Time{}
	for {
		var lines chan string
		var dead, monDead chan struct{}
		if e.conn != nil {
			lines, dead = e.conn.lines, e.conn.dead
		}
		if e.monitor != nil {
			monDead = e.monitor.dead
		}
		select {
		case <-stop:
			e.shutdown()
			return
		case l := <-lines:
			e.handleLine(l)
		case <-dead:
			e.dropConn("USB connection lost")
		case <-monDead:
			e.logf("Serial monitor: port closed")
			e.stopMonitor()
		case r := <-e.probeRes:
			e.probing = false
			if e.pendingUpload != nil {
				if r.conn != nil {
					r.conn.close()
				}
				req := e.pendingUpload
				e.pendingUpload = nil
				e.beginUpload(*req)
				continue
			}
			if r.conn == nil {
				e.nextProbe = e.now().Add(3 * time.Second)
				if strings.Join(r.report, "|") != strings.Join(e.probeReport, "|") {
					for _, l := range r.report {
						e.logf("Search: %s", l)
					}
				}
				e.probeReport = r.report
				if e.status == "" || strings.HasPrefix(e.status, "Looking") || strings.HasPrefix(e.status, "Starting") ||
					strings.HasPrefix(e.status, "Mixer not found") {
					e.status = "Mixer not found. " + strings.Join(r.report, " · ")
				}
			} else if e.paused || e.conn != nil {
				r.conn.close()
			} else {
				e.probeReport = nil
				e.adopt(r.conn)
			}
		case c := <-e.cmds:
			c.reply <- e.handleCmd(c)
		case <-tick.C:
			e.onTick()
			if e.now().Sub(lastPub) >= 100*time.Millisecond {
				lastPub = e.now()
				e.publish(e.snapshot())
			}
		}
	}
}

func (e *Engine) shutdown() {
	e.stopMonitor()
	if e.conn != nil {
		e.conn.port.Write(Command("L,0,0,0,0,0,0"))
		e.conn.close()
	}
	if e.audio != nil {
		e.audio.Close()
	}
}

// ---------- connection ----------

// PortInfo is a serial port and Windows' description of it.
type PortInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

func likelyNano(p PortInfo) bool {
	d := strings.ToLower(p.Desc)
	for _, k := range []string{"ch340", "ch341", "arduino", "usb-serial", "usb serial", "ftdi", "ft232", "cp210", "usb"} {
		if strings.Contains(d, k) {
			return true
		}
	}
	return false
}

func (e *Engine) candidates() []string {
	ports := e.plat.ListPorts()
	e.portsCache = ports
	if e.cfg.Port != "auto" {
		return []string{e.cfg.Port}
	}
	var first, rest []string
	for _, p := range ports {
		switch {
		case p.Name == e.lastGood:
		case strings.Contains(strings.ToLower(p.Desc), "bluetooth"):
			// opening Bluetooth serial ports can hang for many seconds; never auto-probe them
		case likelyNano(p):
			first = append(first, p.Name)
		default:
			rest = append(rest, p.Name)
		}
	}
	out := []string{}
	if e.lastGood != "" {
		out = append(out, e.lastGood)
	}
	return append(append(out, first...), rest...)
}

type probeResult struct {
	conn   *conn
	report []string
}

func (e *Engine) startProbe() {
	e.probing = true
	cands := e.candidates()
	if e.status == "" || !strings.HasPrefix(e.status, "Mixer not found") {
		e.status = "Looking for the mixer…"
	}
	go func() {
		var report []string
		if len(cands) == 0 {
			report = append(report, "Windows reports no COM ports")
		}
		for _, name := range cands {
			c, why := probePort(e.plat, name)
			if c != nil {
				e.probeRes <- probeResult{conn: c}
				return
			}
			report = append(report, name+": "+why)
		}
		e.probeRes <- probeResult{report: report}
	}()
}

// probePort opens a port and waits for the firmware's HELLO. Opening resets the Nano; its bootloader
// runs for 0.5-2 s, so "?" is only sent after 1.5 s (the old bootloader treats early bytes as commands)
// and we wait up to 4.5 s in total. On failure it says why.
func probePort(p Platform, name string) (*conn, string) {
	port, err := p.OpenPort(name)
	if err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "access is denied") {
			msg = "in use by another program (close Arduino Serial Monitor or other mixer apps)"
		}
		return nil, "could not open: " + msg
	}
	c := &conn{name: name, port: port, lines: make(chan string, 1024), dead: make(chan struct{})}
	var bytesMu sync.Mutex
	got := 0
	var sample []byte
	go func() {
		var sp LineSplitter
		buf := make([]byte, 512)
		for {
			n, err := port.Read(buf)
			if err != nil {
				close(c.dead)
				return
			}
			bytesMu.Lock()
			got += n
			if len(sample) < 40 {
				sample = append(sample, buf[:n]...)
			}
			bytesMu.Unlock()
			sp.Feed(buf[:n], func(l string) {
				select {
				case c.lines <- l:
				default: // engine stalled; drop rather than block the reader
				}
			})
		}
	}()
	deadline := time.After(4500 * time.Millisecond)
	firstAsk := time.After(1500 * time.Millisecond)
	var ask <-chan time.Time
	var ticker *time.Ticker
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	for {
		select {
		case l := <-c.lines:
			l = strings.TrimSpace(l)
			if l == helloLine {
				return c, ""
			}
			if strings.HasPrefix(l, helloPrefix) {
				c.close()
				return nil, "older Deej Mixer firmware (v" + strings.TrimPrefix(l, helloPrefix) + ") — click Upload firmware below to update it"
			}
			if strings.HasPrefix(l, "DEEJCTRL") {
				c.close()
				return nil, "old ChatGPT DeejControl firmware — click Upload firmware below to replace it"
			}
		case <-firstAsk:
			port.Write([]byte("?\n"))
			ticker = time.NewTicker(700 * time.Millisecond)
			ask = ticker.C
		case <-ask:
			port.Write([]byte("?\n"))
		case <-c.dead:
			c.close()
			return nil, "port closed while reading"
		case <-deadline:
			c.close()
			bytesMu.Lock()
			defer bytesMu.Unlock()
			if got == 0 {
				return nil, "opened, but no data arrived (not running the Deej Mixer firmware?)"
			}
			return nil, fmt.Sprintf("received %d bytes but no Deej Mixer greeting (other firmware?): %q", got, sanitize(sample))
		}
	}
}

func sanitize(b []byte) string {
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c >= 32 && c < 127 {
			out = append(out, rune(c))
		} else {
			out = append(out, '·')
		}
	}
	return string(out)
}

func (e *Engine) adopt(c *conn) {
	e.conn = c
	e.lastGood = c.name
	e.lastFrame = e.now()
	e.colorsSent = false
	e.lastLED = ""
	e.haveValues = false
	e.haveVal = [5]bool{}
	e.applied = map[string]bool{}
	e.held = [6]bool{}
	e.status = "Connected on " + c.name
	e.logf("Mixer connected on %s", c.name)
	if e.test.Active {
		e.write(fmt.Sprintf("T,%d", e.test.Pattern))
	}
}

func (e *Engine) dropConn(why string) {
	if e.conn == nil {
		return
	}
	e.logf("%s (%s)", why, e.conn.name)
	e.conn.close()
	e.conn = nil
	e.status = why + ". Reconnecting…"
	e.nextProbe = e.now().Add(1 * time.Second)
	e.held = [6]bool{}
}

func (e *Engine) write(body string) {
	if e.conn == nil {
		return
	}
	if _, err := e.conn.port.Write(Command(body)); err != nil {
		e.dropConn("Write to mixer failed")
	}
}

// ---------- input ----------

func (e *Engine) handleLine(l string) {
	m := ParseLine(l)
	switch m.Kind {
	case MsgFrame:
		e.lastFrame = e.now()
		e.values, e.mask, e.haveValues = m.Values, m.Mask, true
		if e.test.Active {
			e.test.Frames++
			for i, v := range m.Values {
				if v < e.test.Min[i] {
					e.test.Min[i] = v
				}
				if v > e.test.Max[i] {
					e.test.Max[i] = v
				}
			}
		} else {
			e.applyVolumes()
		}
	case MsgPress:
		e.onPress(m.Button)
	case MsgRelease:
		e.onRelease(m.Button)
	}
}

func (e *Engine) onPress(i int) {
	e.held[i], e.pressedAt[i], e.shortDone[i], e.holdDone[i] = true, e.now(), false, false
	if e.test.Active {
		e.test.Presses[i]++
		return
	}
	if e.cfg.Buttons[i].HoldAction == ActNone {
		e.shortDone[i] = true
		e.runAction(i, false)
	}
}

func (e *Engine) onRelease(i int) {
	if !e.held[i] {
		return
	}
	e.held[i] = false
	if e.test.Active {
		e.test.Releases[i]++
		return
	}
	if !e.shortDone[i] && !e.holdDone[i] {
		e.runAction(i, false)
	}
}

func (e *Engine) checkHolds() {
	if e.test.Active {
		return
	}
	for i := range e.held {
		b := e.cfg.Buttons[i]
		if e.held[i] && !e.holdDone[i] && !e.shortDone[i] && b.HoldAction != ActNone &&
			e.now().Sub(e.pressedAt[i]) >= time.Duration(e.cfg.HoldMs)*time.Millisecond {
			e.holdDone[i] = true
			e.runAction(i, true)
		}
	}
}

func (e *Engine) runAction(i int, hold bool) {
	b := e.cfg.Buttons[i]
	action, value, kind := b.Action, b.Value, "press"
	if hold {
		action, value, kind = b.HoldAction, b.HoldValue, "hold"
	}
	name := ButtonNames[i]
	switch action {
	case ActMute:
		ch, _ := strconv.Atoi(value)
		muted := e.toggleMute(ch)
		e.logf("%s %s: %s %s", name, kind, e.cfg.Pots[ch].Label, map[bool]string{true: "muted", false: "unmuted"}[muted])
	case ActMuteMic:
		if e.snap == nil || e.snap.Mic == nil {
			e.logf("%s %s: no microphone found", name, kind)
			return
		}
		m, _ := e.snap.Mic.Muted()
		if err := e.snap.Mic.SetMute(!m); err != nil {
			e.logf("Microphone mute failed: %v", err)
		} else {
			e.logf("%s %s: microphone %s", name, kind, map[bool]string{true: "muted", false: "unmuted"}[!m])
		}
	case ActLaunch:
		e.logf("%s %s: launching %s", name, kind, value)
		go func() {
			if err := e.plat.Launch(value); err != nil {
				log.Printf("launch %s: %v", value, err)
			}
		}()
	case ActHotkey:
		if vks, err := ParseHotkey(value); err == nil {
			e.plat.SendKeys(vks)
			e.logf("%s %s: sent %s", name, kind, value)
		}
	case ActPlayPause, ActNext, ActPrev:
		vk := map[string]uint16{ActPlayPause: 0xB3, ActNext: 0xB0, ActPrev: 0xB1}[action]
		e.plat.SendKeys([]uint16{vk})
		e.logf("%s %s: %s", name, kind, strings.ReplaceAll(action, "_", " "))
	}
}

// ---------- audio ----------

func (e *Engine) refreshAudio() {
	e.lastAudio = e.now()
	if e.audio == nil {
		return
	}
	snap, err := e.audio.Refresh()
	if err != nil {
		e.audioErr = "Audio refresh failed: " + err.Error()
		e.snap = nil
		return
	}
	e.audioErr = ""
	if snap.Master == nil {
		e.audioErr = "No default speaker/headphone device found"
	}
	e.snap = snap
	e.game = NewestGame(snap.Procs, e.cfg.ExtraGameList())
	e.mapSessions()
	if e.haveValues && !e.test.Active {
		e.applyVolumes() // new sessions pick up their knob position immediately
	}
}

func (e *Engine) targets() []string {
	t := make([]string, 5)
	for i, p := range e.cfg.Pots {
		t[i] = p.Target
	}
	return t
}

func (e *Engine) mapSessions() {
	if e.snap == nil {
		e.potSess = [5][]int{}
		return
	}
	t := e.targets()
	for i := range e.potSess {
		e.potSess[i] = SessionsFor(i, t, e.snap.Sessions, e.game)
	}
}

type keyedCtl struct {
	key string
	ctl VolumeCtl
}

func (e *Engine) ctlsFor(pot int) []keyedCtl {
	if e.snap == nil {
		return nil
	}
	var out []keyedCtl
	t := e.cfg.Pots[pot].Target
	if hasPart(t, TargetMaster) && e.snap.Master != nil {
		out = append(out, keyedCtl{"endpoint:master", e.snap.Master})
	}
	if hasPart(t, TargetMic) && e.snap.Mic != nil {
		out = append(out, keyedCtl{"endpoint:mic", e.snap.Mic})
	}
	for _, si := range e.potSess[pot] {
		out = append(out, keyedCtl{e.snap.Sessions[si].Key, e.snap.Ctls[si]})
	}
	return out
}

func potLevel(raw int, reverse bool) float64 {
	v := float64(raw) / 1023
	if reverse {
		v = 1 - v
	}
	return math.Max(0, math.Min(1, v))
}

// applyVolumes sets volumes only for knobs that moved, or for audio sessions that appeared since the
// last frame. Volume changes made elsewhere in Windows stay put until the knob is touched.
func (e *Engine) applyVolumes() {
	if !e.haveValues {
		return
	}
	seen := map[string]bool{}
	for i, p := range e.cfg.Pots {
		v := potLevel(e.values[i], p.Reverse)
		moved := !e.haveVal[i] || math.Abs(v-e.lastVal[i]) >= 0.004 || ((v == 0 || v == 1) && v != e.lastVal[i])
		if moved {
			e.lastVal[i], e.haveVal[i] = v, true
		}
		for _, kc := range e.ctlsFor(i) {
			key := fmt.Sprintf("%d|%s", i, kc.key)
			seen[key] = true
			if moved || !e.applied[key] {
				if err := kc.ctl.SetVolume(float32(v)); err != nil {
					continue
				}
				if !e.applied[key] && e.wantMute[i] {
					kc.ctl.SetMute(true)
				}
				e.applied[key] = true
			}
		}
	}
	for k := range e.applied {
		if !seen[k] {
			delete(e.applied, k)
		}
	}
}

func (e *Engine) potMuted(pot int) (muted, active bool) {
	ctls := e.ctlsFor(pot)
	if len(ctls) == 0 {
		return e.wantMute[pot], false
	}
	for _, kc := range ctls {
		if m, err := kc.ctl.Muted(); err != nil || !m {
			return false, true
		}
	}
	return true, true
}

func (e *Engine) toggleMute(pot int) bool {
	muted, active := e.potMuted(pot)
	next := !muted
	if active {
		for _, kc := range e.ctlsFor(pot) {
			kc.ctl.SetMute(next)
		}
	}
	e.wantMute[pot] = next
	return next
}

// ---------- LEDs ----------

func (e *Engine) ledState() (body string, colors [5]string) {
	c := e.cfg
	for i := 0; i < 4; i++ {
		colors[i] = c.LogoColors[i]
	}
	colors[4] = c.RingColor
	masterMuted := false
	micMuted := false
	if e.snap != nil && e.snap.Master != nil {
		masterMuted, _ = e.snap.Master.Muted()
	}
	if e.snap != nil && e.snap.Mic != nil {
		micMuted, _ = e.snap.Mic.Muted()
	}
	if c.MicIndicator && micMuted {
		colors[4] = c.MicColor
	}
	level := 0
	if m, _ := e.potMuted(0); !m && !masterMuted && e.haveValues {
		level = int(math.Ceil(potLevel(e.values[0], c.Pots[0].Reverse)*7 - 1e-9))
	}
	var st [4]int
	for i := 0; i < 4; i++ {
		ch := c.LogoChannel[i]
		muted, active := e.potMuted(ch)
		switch {
		case muted || masterMuted:
			st[i] = 0
		case active:
			st[i] = 2
		default:
			st[i] = map[string]int{"off": 0, "dim": 1, "on": 2}[c.IdleLogo]
		}
	}
	ringB := c.Brightness * 160 / 100 // firmware caps the ring at 160/255 for USB power
	logoB := 255                      // logo LEDs always run at full brightness when on
	return fmt.Sprintf("L,%d,%d,%d,%d,%d,%d,%d", level, st[0], st[1], st[2], st[3], ringB, logoB), colors
}

func (e *Engine) updateLEDs() {
	if e.conn == nil {
		return
	}
	body, colors := e.ledState()
	for i, col := range colors {
		if !e.colorsSent || col != e.lastColors[i] {
			e.write(fmt.Sprintf("C,%d,%s", i, strings.TrimPrefix(col, "#")))
			e.lastColors[i] = col
		}
	}
	e.colorsSent = true
	if body != e.lastLED || e.now().Sub(e.lastLEDSent) >= 500*time.Millisecond {
		if e.test.Active {
			e.write(fmt.Sprintf("T,%d", e.test.Pattern)) // keep-alive in test mode
		} else {
			e.write(body)
		}
		e.lastLED, e.lastLEDSent = body, e.now()
	}
}

// ---------- tick, commands, snapshot ----------

func (e *Engine) onTick() {
	now := e.now()
	if e.conn == nil && !e.probing && !e.paused && !now.Before(e.nextProbe) {
		e.startProbe()
	}
	if e.conn != nil && now.Sub(e.lastFrame) > 3*time.Second {
		e.dropConn("Mixer stopped sending data")
	}
	if now.Sub(e.lastAudio) >= 2*time.Second {
		e.refreshAudio()
	}
	e.checkHolds()
	if now.Sub(e.lastLEDEval) >= 100*time.Millisecond {
		e.lastLEDEval = now
		e.updateLEDs()
	}
}

func (e *Engine) handleCmd(c engineCmd) error {
	switch c.kind {
	case "config":
		portChanged := c.cfg.Port != e.cfg.Port
		e.cfg = c.cfg
		e.haveVal = [5]bool{}
		e.applied = map[string]bool{}
		e.colorsSent = false
		e.game = nil
		if e.snap != nil {
			e.game = NewestGame(e.snap.Procs, e.cfg.ExtraGameList())
		}
		e.mapSessions()
		if portChanged && e.conn != nil {
			e.dropConn("Port setting changed")
			e.nextProbe = e.now()
		}
		e.logf("Settings saved")
	case "test_start":
		e.test = TestState{Active: true, Pattern: 1}
		for i := range e.test.Min {
			e.test.Min[i] = 1023
		}
		e.held = [6]bool{}
		e.write("T,1")
		e.logf("Hardware test started (volumes and buttons paused)")
	case "test_stop":
		if e.test.Active {
			e.test.Active = false
			e.write("T,0")
			e.haveVal = [5]bool{}
			e.lastLED = ""
			e.logf("Hardware test finished")
		}
	case "test_reset":
		if e.test.Active {
			p := e.test.Pattern
			e.test = TestState{Active: true, Pattern: p}
			for i := range e.test.Min {
				e.test.Min[i] = 1023
			}
		}
	case "pattern":
		if c.n < 0 || c.n > 5 {
			return fmt.Errorf("bad pattern")
		}
		e.test.Pattern = c.n
		if e.test.Active {
			e.write(fmt.Sprintf("T,%d", c.n))
		}
	case "run":
		if c.n < 0 || c.n > 11 {
			return fmt.Errorf("bad button")
		}
		e.runAction(c.n%6, c.n >= 6)
	case "reconnect":
		if e.conn != nil {
			e.dropConn("Reconnecting")
		}
		e.nextProbe = e.now()
	case "upload":
		return e.startUpload(uploadReq{port: c.text, hex: c.hex, done: c.done})
	case "upload_done":
		e.uploading, e.paused = false, false
		e.nextProbe = e.now().Add(500 * time.Millisecond)
		var err error
		if c.text == "" {
			e.status = "Firmware uploaded and verified. Reconnecting…"
			e.logf("Firmware upload verified")
		} else {
			err = fmt.Errorf("%s", c.text)
			e.status = "Firmware upload failed: " + c.text
			e.logf("Firmware upload failed: %s", c.text)
		}
		if e.uploadDone != nil {
			e.uploadDone(err)
			e.uploadDone = nil
		}
	case "monitor_start":
		return e.startMonitor(c.text)
	case "monitor_stop":
		e.stopMonitor()
	case "monitor_send":
		if e.monitor == nil {
			return fmt.Errorf("serial monitor is not open")
		}
		if _, err := e.monitor.port.Write([]byte(c.text + "\n")); err != nil {
			return err
		}
	case "ports":
		e.portsCache = e.plat.ListPorts()
	}
	return nil
}

func (e *Engine) resolvePort(port string) (string, error) {
	if e.conn != nil {
		return e.conn.name, nil
	}
	if port != "" && port != "auto" {
		return port, nil
	}
	if e.cfg.Port != "auto" {
		return e.cfg.Port, nil
	}
	if e.lastGood != "" {
		return e.lastGood, nil
	}
	for _, p := range e.portsCache {
		if likelyNano(p) {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("choose the Nano's COM port first")
}

func (e *Engine) startUpload(req uploadReq) error {
	if e.uploading || e.pendingUpload != nil {
		return fmt.Errorf("an upload is already running")
	}
	port, err := e.resolvePort(req.port)
	if err != nil {
		return err
	}
	req.port = port
	e.stopMonitor()
	if e.conn != nil {
		e.conn.close()
		e.conn = nil
	}
	e.paused = true
	if e.probing { // a search has the port open; start as soon as it finishes
		e.pendingUpload = &req
		e.status = "Preparing upload…"
		return nil
	}
	e.beginUpload(req)
	return nil
}

func (e *Engine) beginUpload(req uploadReq) {
	e.uploading = true
	e.uploadDone = req.done
	e.uploadMu.Lock()
	e.uploadLog.Reset()
	e.uploadMu.Unlock()
	e.status = "Uploading firmware to " + req.port + "…"
	e.logf("Uploading firmware to %s", req.port)
	go func() {
		err := e.plat.UploadFirmware(req.port, req.hex, func(s string) {
			e.uploadMu.Lock()
			e.uploadLog.WriteString(s)
			e.uploadMu.Unlock()
		})
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		e.cmds <- engineCmd{kind: "upload_done", text: msg, reply: make(chan error, 1)}
	}()
}

func (e *Engine) UploadLog() string {
	e.uploadMu.Lock()
	defer e.uploadMu.Unlock()
	return e.uploadLog.String()
}

// startMonitor releases the mixer connection and opens the port as a plain 115200-baud serial monitor.
func (e *Engine) startMonitor(port string) error {
	if e.uploading {
		return fmt.Errorf("wait for the upload to finish")
	}
	if e.monitor != nil {
		return nil
	}
	port, err := e.resolvePort(port)
	if err != nil {
		return err
	}
	if e.conn != nil {
		e.conn.close()
		e.conn = nil
	}
	e.paused = true
	p, err := e.plat.OpenPort(port)
	if err != nil {
		e.paused = false
		return fmt.Errorf("could not open %s: %v", port, err)
	}
	c := &conn{name: port, port: p, lines: make(chan string, 1), dead: make(chan struct{})}
	e.monitor = c
	e.monMu.Lock()
	e.monLines, e.monSeq = nil, 0
	e.monMu.Unlock()
	e.status = "Serial monitor open on " + port + " (mixer paused)"
	go func() {
		var sp LineSplitter
		buf := make([]byte, 512)
		for {
			n, err := p.Read(buf)
			if err != nil {
				close(c.dead)
				return
			}
			sp.Feed(buf[:n], func(l string) {
				e.monMu.Lock()
				e.monLines = append(e.monLines, l)
				if len(e.monLines) > 2000 {
					e.monLines = e.monLines[len(e.monLines)-2000:]
				}
				e.monSeq++
				e.monMu.Unlock()
			})
		}
	}()
	return nil
}

func (e *Engine) stopMonitor() {
	if e.monitor == nil {
		return
	}
	e.monitor.close()
	e.monitor = nil
	e.paused = e.uploading || e.pendingUpload != nil
	e.nextProbe = e.now().Add(300 * time.Millisecond)
	e.status = "Serial monitor closed. Reconnecting…"
}

// MonitorSince returns monitor lines after sequence number `seq` and the new sequence number.
func (e *Engine) MonitorSince(seq int) ([]string, int) {
	e.monMu.Lock()
	defer e.monMu.Unlock()
	first := e.monSeq - len(e.monLines) // sequence number of monLines[0]
	if seq < first {
		seq = first
	}
	if seq > e.monSeq {
		seq = e.monSeq
	}
	return append([]string(nil), e.monLines[seq-first:]...), e.monSeq
}

func (e *Engine) snapshot() Snapshot {
	s := Snapshot{Connected: e.conn != nil, Status: e.status, Values: e.values, Mask: e.mask,
		AudioError: e.audioErr, Test: e.test, Uploading: e.uploading, Ports: e.portsCache, ProbeReport: e.probeReport}
	s.Log = append([]string(nil), e.logLines...)
	s.UploadLog = e.UploadLog()
	if e.monitor != nil {
		s.Monitor, s.MonitorPort = true, e.monitor.name
	}
	e.monMu.Lock()
	s.MonitorSeq = e.monSeq
	e.monMu.Unlock()
	if e.conn != nil {
		s.Port = e.conn.name
	}
	if e.game != nil {
		s.Game = e.game.Title
	}
	if e.snap != nil {
		if e.snap.Master != nil {
			s.MasterMuted, _ = e.snap.Master.Muted()
		}
		if e.snap.Mic != nil {
			s.MicMuted, _ = e.snap.Mic.Muted()
		}
		seen := map[string]bool{}
		for _, ss := range e.snap.Sessions {
			n := ss.Name
			if ss.System {
				n = TargetSystem
			}
			if n != "" && !seen[n] {
				seen[n] = true
				s.AllApps = append(s.AllApps, n)
			}
		}
	}
	for i := range s.Pots {
		lv := PotLive{Percent: int(math.Round(potLevel(e.values[i], e.cfg.Pots[i].Reverse) * 100))}
		lv.Muted, lv.Active = e.potMuted(i)
		t := e.cfg.Pots[i].Target
		if hasPart(t, TargetMaster) && e.snap != nil && e.snap.Master != nil {
			lv.Sessions = append(lv.Sessions, "speakers")
		}
		if hasPart(t, TargetMic) && e.snap != nil && e.snap.Mic != nil {
			lv.Sessions = append(lv.Sessions, "microphone")
		}
		if e.snap != nil {
			seen := map[string]bool{}
			for _, si := range e.potSess[i] {
				n := e.snap.Sessions[si].Name
				if e.snap.Sessions[si].System {
					n = "system sounds"
				}
				if !seen[n] {
					seen[n] = true
					lv.Sessions = append(lv.Sessions, n)
				}
			}
		}
		s.Pots[i] = lv
	}
	return s
}
