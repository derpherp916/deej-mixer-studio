package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"2.0.1", "2.0.0", true}, {"v2.1.0", "2.0.9", true}, {"2.0.0", "2.0.0", false}, {"1.9.9", "2.0.0", false},
		{"10.0.0", "9.9.9", true}, {"2.0.0-beta", "2.0.0", false}}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

type fakeGitHub struct {
	srv      *httptest.Server
	setup    []byte
	sums     string
	sig      string
	tag      string
	apiCalls int
}

func newFakeGitHub(t *testing.T, tag string, priv ed25519.PrivateKey) *fakeGitHub {
	f := &fakeGitHub{tag: tag, setup: []byte("MZ fake installer " + tag)}
	h := sha256.Sum256(f.setup)
	name := "DeejMixer-Setup-" + strings.TrimPrefix(tag, "v") + ".exe"
	f.sums = fmt.Sprintf("%s  %s\n%s  DeejMixer-firmware.hex\n", hex.EncodeToString(h[:]), name, strings.Repeat("0", 64))
	if priv != nil {
		f.sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(f.sums)))
	}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/repos/me/deej-mixer-studio/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls++
		assets := []map[string]any{
			{"name": name, "browser_download_url": f.srv.URL + "/dl/setup", "size": len(f.setup)},
			{"name": "SHA256SUMS.txt", "browser_download_url": f.srv.URL + "/dl/sums"},
		}
		if f.sig != "" {
			assets = append(assets, map[string]any{"name": "SHA256SUMS.txt.sig", "browser_download_url": f.srv.URL + "/dl/sig"})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "body": "- Brighter LEDs", "html_url": "https://example/rel", "assets": assets})
	})
	mux.HandleFunc("/dl/setup", func(w http.ResponseWriter, r *http.Request) { w.Write(f.setup) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(f.sums)) })
	mux.HandleFunc("/dl/sig", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(f.sig)) })
	t.Cleanup(f.srv.Close)
	return f
}

func withUpdateConfig(t *testing.T, api, repo, pub string) {
	oa, or, op, ov := githubAPI, UpdateRepo, UpdatePubKey, AppVersion
	githubAPI, UpdateRepo, UpdatePubKey, AppVersion = api, repo, pub, "2.0.0"
	t.Cleanup(func() { githubAPI, UpdateRepo, UpdatePubKey, AppVersion = oa, or, op, ov })
}

func TestUpdaterSignedFlowAndOneTimeNotification(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	gh := newFakeGitHub(t, "v2.1.0", priv)
	withUpdateConfig(t, gh.srv.URL, "me/deej-mixer-studio", base64.StdEncoding.EncodeToString(pub))
	dir := t.TempDir()
	notes := 0
	installed := ""
	u := NewUpdater(dir, func(title, text string) { notes++ }, func(p string) error { installed = p; return nil })
	st, err := u.Check(true)
	if err != nil || !st.Available || st.Latest != "2.1.0" || notes != 1 {
		t.Fatalf("check: %+v %v notes=%d", st, err, notes)
	}
	u.Check(true)
	u2 := NewUpdater(dir, func(string, string) { notes++ }, nil) // restart: still only one notification
	u2.Check(true)
	if notes != 1 {
		t.Fatalf("notification must be shown once per version, got %d", notes)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(installed)
	if string(data) != string(gh.setup) || !strings.HasSuffix(installed, "DeejMixer-Setup-2.1.0.exe") {
		t.Fatalf("installed %q", installed)
	}
	// Tampered installer is refused.
	gh.setup = []byte("MZ evil")
	if err := u.Install(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered download must be refused: %v", err)
	}
	// Forged checksums (signature no longer matches) are refused.
	gh.sums = strings.Replace(gh.sums, gh.sums[:64], strings.Repeat("a", 64), 1)
	if err := u.Install(); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("bad signature must be refused: %v", err)
	}
}

func TestUpdaterUnsignedRefusedWhenKeyConfigured(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	gh := newFakeGitHub(t, "v3.0.0", nil)
	withUpdateConfig(t, gh.srv.URL, "me/deej-mixer-studio", base64.StdEncoding.EncodeToString(pub))
	u := NewUpdater(t.TempDir(), nil, func(string) error { t.Fatal("must not install"); return nil })
	u.Check(false)
	if err := u.Install(); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdaterUpToDateAndDisabled(t *testing.T) {
	gh := newFakeGitHub(t, "v2.0.0", nil)
	withUpdateConfig(t, gh.srv.URL, "me/deej-mixer-studio", "")
	notes := 0
	u := NewUpdater(t.TempDir(), func(string, string) { notes++ }, nil)
	st, _ := u.Check(true)
	if st.Available || notes != 0 {
		t.Fatalf("same version must not be offered: %+v", st)
	}
	UpdateRepo = ""
	u2 := NewUpdater(t.TempDir(), nil, nil)
	if _, err := u2.Check(true); err == nil || u2.State().Enabled {
		t.Fatal("local builds have updates off")
	}
}
