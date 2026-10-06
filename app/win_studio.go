//go:build windows

package main

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const cliDownloadURL = "https://downloads.arduino.cc/arduino-cli/arduino-cli_latest_Windows_64bit.zip"

type winStudioTools struct{}

// FindCLI looks for arduino-cli next to the app, inside an Arduino IDE 2 install, or in the app's own
// tools folder; if none is found it downloads the official build once (about 15 MB).
func (winStudioTools) FindCLI(logf func(string)) (string, error) {
	own := filepath.Join(dataDir(), "arduino-cli", "arduino-cli.exe")
	cands := []string{filepath.Join(exeDir(), "tools", "arduino-cli.exe"), own}
	for _, env := range []string{"ProgramFiles", "LOCALAPPDATA", "ProgramFiles(x86)"} {
		if b := os.Getenv(env); b != "" {
			cands = append(cands,
				filepath.Join(b, "Arduino IDE", "resources", "app", "lib", "backend", "resources", "arduino-cli.exe"),
				filepath.Join(b, "Programs", "Arduino IDE", "resources", "app", "lib", "backend", "resources", "arduino-cli.exe"))
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			logf("Using " + c + "\n")
			return c, nil
		}
	}
	logf("Arduino tools not found on this PC. Downloading arduino-cli from arduino.cc…\n")
	if err := downloadCLI(own, logf); err != nil {
		return "", fmt.Errorf("could not download arduino-cli: %v", err)
	}
	return own, nil
}

func downloadCLI(dst string, logf func(string)) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(cliDownloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("server answered %s", resp.Status)
	}
	tmp, err := os.CreateTemp("", "arduino-cli-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, 200<<20))
	tmp.Close()
	if err != nil {
		return err
	}
	logf(fmt.Sprintf("Downloaded %.1f MB\n", float64(n)/1e6))
	zr, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Base(f.Name), "arduino-cli.exe") {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			os.MkdirAll(filepath.Dir(dst), 0o755)
			out, err := os.Create(dst + ".new")
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(rc, 300<<20)); err != nil {
				out.Close()
				return err
			}
			out.Close()
			os.Remove(dst)
			return os.Rename(dst+".new", dst)
		}
	}
	return errors.New("arduino-cli.exe was not in the download")
}

// Run executes a program without a console window, streaming its output line by line.
func (winStudioTools) Run(exe string, args []string, logf func(string)) (string, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var all strings.Builder
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text() + "\n"
			mu.Lock()
			if all.Len() < 4<<20 {
				all.WriteString(line)
			}
			mu.Unlock()
			if logf != nil {
				logf(line)
			}
		}
		io.Copy(io.Discard, pr)
		close(done)
	}()
	if err := cmd.Start(); err != nil {
		pw.Close()
		<-done
		return "", err
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	var err error
	select {
	case err = <-waitErr:
	case <-time.After(15 * time.Minute):
		cmd.Process.Kill()
		err = errors.New("timed out")
	}
	pw.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return all.String(), err
}
