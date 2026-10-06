package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var indexHTML []byte

//go:embed res/app.ico
var faviconICO []byte

//go:embed ui/editor.js
var editorJS []byte

// Hooks are OS integrations the UI can trigger.
type Hooks struct {
	StartupEnabled func() bool
	SetStartup     func(bool) error
	Install        func() (string, error)
	IsInstalled    func() bool
	OpenDrivers    func() error
	OpenPath       func(string) error
}

type Server struct {
	engine  *Engine
	dataDir string
	hooks   Hooks
	token   string
	addr    string

	studio  *Studio
	updater *Updater

	mu       sync.Mutex
	profiles *ProfileStore
	latest   Snapshot
	notify   chan struct{}
}

func NewServer(e *Engine, ps *ProfileStore, dataDir string, hooks Hooks, studio *Studio) *Server {
	b := make([]byte, 16)
	rand.Read(b)
	return &Server{engine: e, profiles: ps, dataDir: dataDir, hooks: hooks, studio: studio,
		token: hex.EncodeToString(b), notify: make(chan struct{})}
}

// Publish is called by the engine with fresh live state.
func (s *Server) Publish(snap Snapshot) {
	s.mu.Lock()
	s.latest = snap
	old := s.notify
	s.notify = make(chan struct{})
	s.mu.Unlock()
	close(old)
}

func (s *Server) URL() string { return fmt.Sprintf("http://%s/?t=%s", s.addr, s.token) }

// Listen binds to localhost only. A fixed port is tried first so the address stays stable.
func (s *Server) Listen() (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:47957")
	if err != nil {
		l, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	s.addr = l.Addr().String()
	return l, nil
}

func (s *Server) Serve(l net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Write(faviconICO)
	})
	mux.HandleFunc("/api/state", s.api(s.getState))
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/config", s.api(s.postConfig))
	mux.HandleFunc("/api/defaults", s.api(s.getDefaults))
	mux.HandleFunc("/api/command", s.api(s.postCommand))
	mux.HandleFunc("/api/report", s.api(s.postReport))
	mux.HandleFunc("/api/profile", s.api(s.postProfile))
	mux.HandleFunc("/api/updates", s.api(s.updates))
	mux.HandleFunc("/api/studio/sketch", s.apiN(s.studioSketch, 600<<10))
	mux.HandleFunc("/api/studio/build", s.api(s.studioBuild))
	mux.HandleFunc("/api/studio/job", s.api(s.studioJob))
	mux.HandleFunc("/api/studio/monitor", s.api(s.studioMonitor))
	mux.HandleFunc("/editor.js", func(w http.ResponseWriter, r *http.Request) { // static, public code
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(editorJS)
	})
	srv := &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	return srv.Serve(l)
}

// guard blocks DNS-rebinding (Host must be our loopback address) and cross-site requests.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.addr && r.Host != strings.Replace(s.addr, "127.0.0.1", "localhost", 1) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tokenOK(r *http.Request) bool {
	t := r.Header.Get("X-Deej-Token")
	if t == "" {
		t = r.URL.Query().Get("t")
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.tokenOK(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<body style="font-family:Segoe UI,sans-serif;background:#111;color:#ddd;padding:40px">
<h2>Deej Mixer</h2><p>Open the control panel from the Deej Mixer tray icon (bottom-right of the taskbar).</p></body>`)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
	w.Write(indexHTML)
}

type apiFunc func(r *http.Request) (any, error)

func (s *Server) api(fn apiFunc) http.HandlerFunc { return s.apiN(fn, 64<<10) }

func (s *Server) apiN(fn apiFunc, limit int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "json required", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		v, err := fn(r)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
}

type stateResp struct {
	Config      Config    `json:"config"`
	Profiles    []string  `json:"profiles"`
	Active      string    `json:"activeProfile"`
	Live        Snapshot  `json:"live"`
	ButtonNames [6]string `json:"buttonNames"`
	ButtonPins  [6]string `json:"buttonPins"`
	Startup     bool      `json:"startup"`
	Installed   bool      `json:"installed"`
	DataDir     string    `json:"dataDir"`
	Version     string    `json:"version"`
}

func (s *Server) getState(r *http.Request) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := stateResp{Config: s.profiles.Current(), Profiles: append([]string(nil), s.profiles.Order...),
		Active: s.profiles.Active, Live: s.latest, ButtonNames: ButtonNames, ButtonPins: ButtonPins,
		DataDir: s.dataDir, Version: AppVersion}
	if s.hooks.StartupEnabled != nil {
		st.Startup = s.hooks.StartupEnabled()
	}
	if s.hooks.IsInstalled != nil {
		st.Installed = s.hooks.IsInstalled()
	}
	return st, nil
}

func (s *Server) getDefaults(r *http.Request) (any, error) { return DefaultConfig(), nil }

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for {
		s.mu.Lock()
		snap, ch := s.latest, s.notify
		s.mu.Unlock()
		data, _ := json.Marshal(snap)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *Server) postConfig(r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("POST required")
	}
	var c Config
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		return nil, fmt.Errorf("bad settings: %v", err)
	}
	c, err := c.Normalize()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.profiles.SetCurrent(c)
	err = s.profiles.Save(s.dataDir)
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("could not save settings: %v", err)
	}
	if err := s.engine.Send(engineCmd{kind: "config", cfg: c}); err != nil {
		return nil, err
	}
	return c, nil
}

type commandReq struct {
	Cmd  string `json:"cmd"`
	N    int    `json:"n"`
	Text string `json:"text"`
	On   bool   `json:"on"`
}

func (s *Server) postCommand(r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("POST required")
	}
	var c commandReq
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		return nil, err
	}
	switch c.Cmd {
	case "test_start", "test_stop", "test_reset", "pattern", "run", "reconnect", "ports":
		return map[string]bool{"ok": true}, s.engine.Send(engineCmd{kind: c.Cmd, n: c.N})
	case "upload":
		return map[string]bool{"ok": true}, s.engine.Send(engineCmd{kind: "upload", text: c.Text})
	case "startup":
		if s.hooks.SetStartup == nil {
			return nil, errors.New("not supported")
		}
		return map[string]bool{"ok": true}, s.hooks.SetStartup(c.On)
	case "install":
		if s.hooks.Install == nil {
			return nil, errors.New("not supported")
		}
		msg, err := s.hooks.Install()
		return map[string]string{"message": msg}, err
	case "drivers":
		if s.hooks.OpenDrivers == nil {
			return nil, errors.New("not supported")
		}
		return map[string]bool{"ok": true}, s.hooks.OpenDrivers()
	case "open_sketch":
		if s.hooks.OpenPath == nil || s.studio == nil {
			return nil, errors.New("not supported")
		}
		s.studio.Load() // make sure it exists
		return map[string]bool{"ok": true}, s.hooks.OpenPath(s.studio.Dir())
	case "open_data":
		if s.hooks.OpenPath == nil {
			return nil, errors.New("not supported")
		}
		return map[string]bool{"ok": true}, s.hooks.OpenPath(s.dataDir)
	}
	return nil, fmt.Errorf("unknown command %q", c.Cmd)
}

// postReport stores the hardware test result next to the settings.
func (s *Server) postReport(r *http.Request) (any, error) {
	var report map[string]any
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		return nil, err
	}
	report["savedAt"] = time.Now().Format(time.RFC3339)
	report["version"] = AppVersion
	data, _ := json.MarshalIndent(report, "", "  ")
	p := filepath.Join(s.dataDir, "hardware-test.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return nil, err
	}
	return map[string]string{"path": p}, nil
}

type profileReq struct {
	Action  string `json:"action"`
	Name    string `json:"name"`
	NewName string `json:"newName"`
}

func (s *Server) postProfile(r *http.Request) (any, error) {
	var p profileReq
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	c, err := s.profiles.Apply(p.Action, p.Name, p.NewName)
	if err == nil {
		err = s.profiles.Save(s.dataDir)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.engine.Send(engineCmd{kind: "config", cfg: c}); err != nil {
		return nil, err
	}
	return s.getState(r)
}

type sketchReq struct {
	Source string `json:"source"`
	Reset  bool   `json:"reset"`
}

func (s *Server) studioSketch(r *http.Request) (any, error) {
	if r.Method == http.MethodPost {
		var q sketchReq
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			return nil, err
		}
		var err error
		if q.Reset {
			err = s.studio.Reset()
		} else {
			err = s.studio.Save(q.Source)
		}
		if err != nil {
			return nil, err
		}
	}
	src, modified, err := s.studio.Load()
	if err != nil {
		return nil, err
	}
	return map[string]any{"source": src, "modified": modified, "path": s.studio.SketchFile()}, nil
}

func (s *Server) studioBuild(r *http.Request) (any, error) {
	var q struct {
		Upload bool `json:"upload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		return nil, err
	}
	return s.studio.Start(q.Upload)
}

func (s *Server) studioJob(r *http.Request) (any, error) { return s.studio.Job(), nil }

type monitorReq struct {
	Action string `json:"action"` // start, stop, send, read
	Port   string `json:"port"`
	Text   string `json:"text"`
	Since  int    `json:"since"`
}

func (s *Server) studioMonitor(r *http.Request) (any, error) {
	var q monitorReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		return nil, err
	}
	switch q.Action {
	case "start":
		if err := s.engine.Send(engineCmd{kind: "monitor_start", text: q.Port}); err != nil {
			return nil, err
		}
	case "stop":
		if err := s.engine.Send(engineCmd{kind: "monitor_stop"}); err != nil {
			return nil, err
		}
	case "send":
		if err := s.engine.Send(engineCmd{kind: "monitor_send", text: q.Text}); err != nil {
			return nil, err
		}
	case "read":
	default:
		return nil, fmt.Errorf("unknown monitor action")
	}
	lines, seq := s.engine.MonitorSince(q.Since)
	return map[string]any{"lines": lines, "seq": seq}, nil
}

// SetUpdater connects the release checker (optional).
func (s *Server) SetUpdater(u *Updater) { s.updater = u }

func (s *Server) updates(r *http.Request) (any, error) {
	if s.updater == nil {
		return UpdateState{Current: AppVersion}, nil
	}
	if r.Method == http.MethodPost {
		var q struct {
			Action string `json:"action"` // check, install, auto
			On     bool   `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			return nil, err
		}
		switch q.Action {
		case "check":
			if _, err := s.updater.Check(false); err != nil {
				return nil, err
			}
		case "install":
			go s.updater.Install()
		case "auto":
			s.updater.SetAutoCheck(q.On)
		default:
			return nil, fmt.Errorf("unknown update action")
		}
	}
	return s.updater.State(), nil
}
