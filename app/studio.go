package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Firmware Studio: an Arduino-IDE-style editor for the mixer's firmware, built into the app.
// Sketches are compiled with arduino-cli (found on the PC or downloaded once) and uploaded with the
// bundled AVRDUDE through the engine, which releases the mixer's port while it works.

//go:embed firmware/DeejMixer.ino
var stockSketch []byte

const studioFQBN = "arduino:avr:nano:cpu=atmega328"

// StudioTools are the OS-specific parts: finding/downloading arduino-cli and running programs.
type StudioTools interface {
	FindCLI(logf func(string)) (string, error)
	Run(exe string, args []string, logf func(string)) (string, error)
}

type StudioJob struct {
	ID      int    `json:"id"`
	Kind    string `json:"kind"` // "verify" or "upload"
	Running bool   `json:"running"`
	OK      bool   `json:"ok"`
	Log     string `json:"log"`
	Seconds int    `json:"seconds"`
}

type Studio struct {
	dir    string // ...\DeejMixer\sketch\DeejMixer
	build  string
	tools  StudioTools
	upload func(hex string, done func(error)) error

	mu      sync.Mutex
	job     StudioJob
	log     strings.Builder
	started time.Time
}

func NewStudio(dataDir string, tools StudioTools, upload func(hex string, done func(error)) error) *Studio {
	return &Studio{dir: filepath.Join(dataDir, "sketch", "DeejMixer"), build: filepath.Join(dataDir, "build"),
		tools: tools, upload: upload}
}

func (s *Studio) SketchFile() string { return filepath.Join(s.dir, "DeejMixer.ino") }
func (s *Studio) Dir() string        { return s.dir }

// Load returns the sketch, creating it from the stock firmware the first time.
func (s *Studio) Load() (src string, modified bool, err error) {
	data, err := os.ReadFile(s.SketchFile())
	if errors.Is(err, os.ErrNotExist) {
		if err = s.Reset(); err != nil {
			return "", false, err
		}
		data = stockSketch
	} else if err != nil {
		return "", false, err
	}
	return string(data), !bytes.Equal(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), stockSketch), nil
}

func (s *Studio) Save(src string) error {
	if len(src) > 512<<10 {
		return errors.New("sketch is too large")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp := s.SketchFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(src), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.SketchFile())
}

func (s *Studio) Reset() error { return s.Save(string(stockSketch)) }

func (s *Studio) Job() StudioJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.job
	j.Log = s.log.String()
	if j.Running {
		j.Seconds = int(time.Since(s.started).Seconds())
	}
	return j
}

func (s *Studio) logf(format string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := fmt.Sprintf(format, a...)
	if s.log.Len() < 2<<20 {
		s.log.WriteString(msg)
	}
}

// Start begins a verify (compile) or upload (compile + flash) job in the background.
func (s *Studio) Start(upload bool) (StudioJob, error) {
	s.mu.Lock()
	if s.job.Running {
		s.mu.Unlock()
		return s.Job(), errors.New("a build is already running")
	}
	kind := "verify"
	if upload {
		kind = "upload"
	}
	s.job = StudioJob{ID: s.job.ID + 1, Kind: kind, Running: true}
	s.log.Reset()
	s.started = time.Now()
	s.mu.Unlock()
	go s.run(upload)
	return s.Job(), nil
}

func (s *Studio) finish(ok bool, err error) {
	if err != nil {
		s.logf("\n✖ %v\n", err)
	}
	s.mu.Lock()
	s.job.Running, s.job.OK = false, ok
	s.job.Seconds = int(time.Since(s.started).Seconds())
	s.mu.Unlock()
}

func (s *Studio) run(upload bool) {
	lf := func(t string) { s.logf("%s", t) }
	s.logf("Preparing Arduino tools…\n")
	cli, err := s.tools.FindCLI(lf)
	if err != nil {
		s.finish(false, err)
		return
	}
	out, err := s.tools.Run(cli, []string{"core", "list"}, nil)
	if err != nil || !strings.Contains(out, "arduino:avr") {
		s.logf("Installing the Arduino AVR board package (first time only, needs internet)…\n")
		if _, err := s.tools.Run(cli, []string{"core", "update-index"}, lf); err != nil {
			s.finish(false, fmt.Errorf("could not download the board index: %v", err))
			return
		}
		if _, err := s.tools.Run(cli, []string{"core", "install", "arduino:avr"}, lf); err != nil {
			s.finish(false, fmt.Errorf("could not install arduino:avr: %v", err))
			return
		}
	}
	out, err = s.tools.Run(cli, []string{"lib", "list"}, nil)
	if err != nil || !strings.Contains(out, "Adafruit NeoPixel") {
		s.logf("Installing the Adafruit NeoPixel library…\n")
		if _, err := s.tools.Run(cli, []string{"lib", "install", "Adafruit NeoPixel"}, lf); err != nil {
			s.finish(false, fmt.Errorf("could not install Adafruit NeoPixel: %v", err))
			return
		}
	}
	os.RemoveAll(s.build)
	s.logf("Compiling %s for Arduino Nano (ATmega328P)…\n", s.SketchFile())
	if _, err := s.tools.Run(cli, []string{"compile", "--fqbn", studioFQBN, "--warnings", "default",
		"--output-dir", s.build, s.dir}, lf); err != nil {
		s.finish(false, errors.New("compilation failed — see the errors above"))
		return
	}
	hex := filepath.Join(s.build, "DeejMixer.ino.hex")
	if _, err := os.Stat(hex); err != nil {
		s.finish(false, errors.New("compiler did not produce DeejMixer.ino.hex"))
		return
	}
	s.logf("\n✔ Compiled successfully.\n")
	if !upload {
		s.finish(true, nil)
		return
	}
	s.logf("\nUploading to the Nano…\n")
	done := make(chan error, 1)
	if err := s.upload(hex, func(err error) { done <- err }); err != nil {
		s.finish(false, err)
		return
	}
	select {
	case err := <-done:
		if err != nil {
			s.finish(false, fmt.Errorf("upload failed: %v", err))
			return
		}
		s.logf("✔ Upload verified. The mixer reconnects automatically if the sketch speaks the Deej Mixer protocol.\n")
		s.finish(true, nil)
	case <-time.After(4 * time.Minute):
		s.finish(false, errors.New("upload timed out"))
	}
}
