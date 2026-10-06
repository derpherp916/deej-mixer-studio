package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Release checking. Both values are set at build time by the release workflow:
//
//	-X main.UpdateRepo=owner/deej-mixer-studio  -X main.UpdatePubKey=<base64 ed25519 public key>
//
// With no UpdateRepo (a local build) update checks are off. With an UpdatePubKey, a release is only
// installed if SHA256SUMS.txt carries a valid signature (SHA256SUMS.txt.sig) from the matching private key.
var (
	UpdateRepo   = ""
	UpdatePubKey = ""
	githubAPI    = "https://api.github.com"
)

type UpdateState struct {
	Enabled     bool   `json:"enabled"` // a release source is configured
	AutoCheck   bool   `json:"autoCheck"`
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	Available   bool   `json:"available"`
	Notes       string `json:"notes"`
	PageURL     string `json:"pageUrl"`
	LastCheck   string `json:"lastCheck"`
	Checking    bool   `json:"checking"`
	Downloading bool   `json:"downloading"`
	Progress    int    `json:"progress"` // percent
	Error       string `json:"error"`
	Repo        string `json:"repo"`
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type githubRelease struct {
	Tag        string         `json:"tag_name"`
	Body       string         `json:"body"`
	HTMLURL    string         `json:"html_url"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type updatePrefs struct {
	AutoCheck       bool   `json:"autoCheck"`
	NotifiedVersion string `json:"notifiedVersion"`
	LastCheck       string `json:"lastCheck"`
}

type Updater struct {
	dataDir string
	client  *http.Client
	notify  func(title, text string)     // one-time Windows notification
	install func(setupPath string) error // runs the downloaded installer

	mu      sync.Mutex
	st      UpdateState
	prefs   updatePrefs
	release *githubRelease
}

func NewUpdater(dataDir string, notify func(string, string), install func(string) error) *Updater {
	u := &Updater{dataDir: dataDir, client: &http.Client{Timeout: 60 * time.Second}, notify: notify, install: install}
	u.prefs = updatePrefs{AutoCheck: true}
	if data, err := os.ReadFile(u.prefsPath()); err == nil {
		json.Unmarshal(data, &u.prefs)
	}
	u.st = UpdateState{Enabled: UpdateRepo != "", AutoCheck: u.prefs.AutoCheck, Current: AppVersion, LastCheck: u.prefs.LastCheck, Repo: UpdateRepo}
	return u
}

func (u *Updater) prefsPath() string { return filepath.Join(u.dataDir, "updates.json") }

func (u *Updater) savePrefs() {
	data, _ := json.MarshalIndent(u.prefs, "", "  ")
	os.MkdirAll(u.dataDir, 0o755)
	os.WriteFile(u.prefsPath(), data, 0o644)
}

func (u *Updater) State() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st
}

func (u *Updater) SetAutoCheck(on bool) {
	u.mu.Lock()
	u.prefs.AutoCheck, u.st.AutoCheck = on, on
	u.savePrefs()
	u.mu.Unlock()
}

// Run checks shortly after start-up (the app starts with Windows) and then once a day.
func (u *Updater) Run(stop <-chan struct{}) {
	if UpdateRepo == "" {
		return
	}
	first := time.NewTimer(45 * time.Second)
	defer first.Stop()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-first.C:
		case <-tick.C:
			u.mu.Lock()
			last, _ := time.Parse(time.RFC3339, u.prefs.LastCheck)
			due := time.Since(last) >= 24*time.Hour
			u.mu.Unlock()
			if !due {
				continue
			}
		}
		if u.State().AutoCheck {
			u.Check(true)
		}
	}
}

// versionNewer reports whether version a is newer than b ("v2.1.0" > "2.0.9").
func versionNewer(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func (u *Updater) get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "DeejMixerStudio/"+AppVersion)
	if strings.HasPrefix(url, githubAPI) {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[0], resp.Status)
	}
	return resp, nil
}

// Check asks GitHub for the latest release. With notifyOnce it shows a single Windows notification
// the first time a given new version is seen; it never repeats for the same version.
func (u *Updater) Check(notifyOnce bool) (UpdateState, error) {
	if UpdateRepo == "" {
		return u.State(), errors.New("this build has no update source")
	}
	u.mu.Lock()
	if u.st.Checking || u.st.Downloading {
		u.mu.Unlock()
		return u.State(), nil
	}
	u.st.Checking, u.st.Error = true, ""
	u.mu.Unlock()

	rel, err := u.fetchLatest()
	u.mu.Lock()
	u.st.Checking = false
	now := time.Now().Format(time.RFC3339)
	u.prefs.LastCheck, u.st.LastCheck = now, now
	if err != nil {
		u.st.Error = "Could not check for updates: " + err.Error()
		u.savePrefs()
		u.mu.Unlock()
		return u.State(), err
	}
	u.release = rel
	latest := strings.TrimPrefix(rel.Tag, "v")
	u.st.Latest, u.st.Notes, u.st.PageURL = latest, rel.Body, rel.HTMLURL
	u.st.Available = versionNewer(latest, AppVersion)
	show := notifyOnce && u.st.Available && u.prefs.NotifiedVersion != latest
	if show {
		u.prefs.NotifiedVersion = latest
	}
	u.savePrefs()
	u.mu.Unlock()
	if show && u.notify != nil {
		u.notify("Deej Mixer "+latest+" is available", "Click to see what's new and update. Your settings are kept.")
	}
	return u.State(), nil
}

func (u *Updater) fetchLatest() (*githubRelease, error) {
	resp, err := u.get(fmt.Sprintf("%s/repos/%s/releases/latest", githubAPI, UpdateRepo))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.Tag == "" || rel.Draft || rel.Prerelease {
		return nil, errors.New("no published release yet")
	}
	return &rel, nil
}

func (rel *githubRelease) asset(match func(string) bool) *releaseAsset {
	for i := range rel.Assets {
		if match(rel.Assets[i].Name) {
			return &rel.Assets[i]
		}
	}
	return nil
}

func (u *Updater) setErr(msg string) error {
	u.mu.Lock()
	u.st.Downloading, u.st.Error = false, msg
	u.mu.Unlock()
	return errors.New(msg)
}

// Install downloads the new installer, verifies it, and starts it. The installer closes this app,
// updates it in place (settings are kept) and starts it again.
func (u *Updater) Install() error {
	u.mu.Lock()
	rel := u.release
	if rel == nil || !u.st.Available {
		u.mu.Unlock()
		return errors.New("no update is available — check for updates first")
	}
	if u.st.Downloading {
		u.mu.Unlock()
		return errors.New("already downloading")
	}
	u.st.Downloading, u.st.Progress, u.st.Error = true, 0, ""
	u.mu.Unlock()

	setup := rel.asset(func(n string) bool {
		return strings.HasPrefix(n, "DeejMixer-Setup") && strings.HasSuffix(strings.ToLower(n), ".exe")
	})
	sums := rel.asset(func(n string) bool { return n == "SHA256SUMS.txt" })
	if setup == nil || sums == nil {
		return u.setErr("the release is missing its installer or SHA256SUMS.txt")
	}
	sumsData, err := u.download(sums.URL, 1<<20, nil)
	if err != nil {
		return u.setErr("download failed: " + err.Error())
	}
	if UpdatePubKey != "" {
		sig := rel.asset(func(n string) bool { return n == "SHA256SUMS.txt.sig" })
		if sig == nil {
			return u.setErr("the release is not signed, so it was not installed")
		}
		sigData, err := u.download(sig.URL, 4096, nil)
		if err != nil {
			return u.setErr("download failed: " + err.Error())
		}
		if err := verifySignature(sumsData, sigData, UpdatePubKey); err != nil {
			return u.setErr("the release signature is invalid, so it was not installed")
		}
	}
	want := checksumFor(sumsData, setup.Name)
	if want == "" {
		return u.setErr("SHA256SUMS.txt has no entry for " + setup.Name)
	}
	data, err := u.download(setup.URL, 200<<20, func(done, total int64) {
		if total > 0 {
			u.mu.Lock()
			u.st.Progress = int(done * 100 / total)
			u.mu.Unlock()
		}
	})
	if err != nil {
		return u.setErr("download failed: " + err.Error())
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return u.setErr("the download is damaged (checksum mismatch), so it was not installed")
	}
	dir := filepath.Join(u.dataDir, "updates")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, setup.Name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		return u.setErr("could not save the installer: " + err.Error())
	}
	u.mu.Lock()
	u.st.Progress = 100
	u.mu.Unlock()
	if err := u.install(path); err != nil {
		return u.setErr("could not start the installer: " + err.Error())
	}
	u.mu.Lock()
	u.st.Downloading = false
	u.mu.Unlock()
	return nil
}

func (u *Updater) download(url string, limit int64, progress func(done, total int64)) ([]byte, error) {
	resp, err := u.get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, errors.New("file is unexpectedly large")
	}
	var buf []byte
	chunk := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if int64(len(buf)) > limit {
			return nil, errors.New("file is unexpectedly large")
		}
		if progress != nil {
			progress(int64(len(buf)), resp.ContentLength)
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// checksumFor finds a file's hash in `sha256sum` output ("<hex>  <name>" or "<hex> *<name>").
func checksumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

func verifySignature(msg, sigFile []byte, pubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubB64))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("bad public key")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigFile)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("bad signature file")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return errors.New("signature does not match")
	}
	return nil
}
