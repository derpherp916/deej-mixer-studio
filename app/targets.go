package main

import (
	"path"
	"strings"
)

// Special knob targets.
const (
	TargetMaster    = "master"     // default speakers
	TargetMic       = "mic"        // default microphone
	TargetSystem    = "system"     // Windows system sounds
	TargetSteamGame = "steam_game" // newest running Steam game (falls back to the Steam client)
	TargetOther     = "other"      // every app not assigned to another knob
)

// ProcInfo describes a running process.
type ProcInfo struct {
	PID     uint32
	Name    string // lower-case exe name
	Path    string // lower-case full path, backslashes
	Created int64  // creation time, any monotonic unit
}

// SessionInfo describes an audio session.
type SessionInfo struct {
	Key    string // unique per session instance
	PID    uint32
	Name   string // lower-case exe name ("" if unknown)
	Path   string
	System bool
}

// Game is the Steam game currently targeted.
type Game struct {
	Title string // install folder name, or exe name for extra games
	Dir   string // lower-case "...\steamapps\common\<folder>\" ("" for extra games)
	Exes  map[string]bool
}

var steamHelpers = map[string]bool{
	"steam.exe": true, "steamwebhelper.exe": true, "steamservice.exe": true, "gameoverlayui.exe": true,
	"gameoverlayui64.exe": true, "steamerrorreporter.exe": true, "steamerrorreporter64.exe": true,
	"crashhandler.exe": true, "crashhandler64.exe": true, "vrserver.exe": true, "vrmonitor.exe": true,
	"vrcompositor.exe": true, "vrdashboard.exe": true, "unitycrashhandler64.exe": true,
	"unitycrashhandler32.exe": true, "crashreportclient.exe": true, "easyanticheat.exe": true,
	"easyanticheat_eos.exe": true, "beservice.exe": true, "beservice_x64.exe": true,
}

const steamMarker = `\steamapps\common\`

// NewestGame picks the Steam game (or extra game exe) that was launched most recently.
// Processes inside the same game folder are grouped, and a group's launch time is its oldest process,
// so a helper that starts late cannot steal the target from a newer game.
func NewestGame(procs []ProcInfo, extras []string) *Game {
	extra := map[string]bool{}
	for _, e := range extras {
		extra[strings.ToLower(strings.TrimSpace(e))] = true
	}
	type group struct {
		g     Game
		start int64
	}
	groups := map[string]*group{}
	for _, p := range procs {
		if p.Name == "" || steamHelpers[p.Name] {
			continue
		}
		key, title, dir := "", "", ""
		if i := strings.Index(p.Path, steamMarker); i >= 0 {
			rest := p.Path[i+len(steamMarker):]
			folder := rest
			if j := strings.Index(rest, `\`); j >= 0 {
				folder = rest[:j]
			}
			if folder == "" {
				continue
			}
			dir = p.Path[:i+len(steamMarker)] + folder + `\`
			key, title = dir, folder
		} else if extra[p.Name] {
			key, title = "exe:"+p.Name, strings.TrimSuffix(p.Name, ".exe")
		} else {
			continue
		}
		gr := groups[key]
		if gr == nil {
			gr = &group{g: Game{Title: title, Dir: dir, Exes: map[string]bool{}}, start: p.Created}
			groups[key] = gr
		}
		if p.Created < gr.start {
			gr.start = p.Created
		}
		gr.g.Exes[p.Name] = true
	}
	var best *group
	for _, gr := range groups {
		if best == nil || gr.start > best.start || (gr.start == best.start && gr.g.Title < best.g.Title) {
			best = gr
		}
	}
	if best == nil {
		return nil
	}
	return &best.g
}

// targetParts splits "a.exe, b.exe" into its parts.
func targetParts(t string) []string {
	var out []string
	for _, p := range strings.Split(t, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func gameOwns(game *Game, s SessionInfo) bool {
	if game == nil {
		return s.Name == "steam.exe" || s.Name == "steamwebhelper.exe"
	}
	if game.Dir != "" && strings.HasPrefix(s.Path, game.Dir) {
		return true
	}
	return game.Dir == "" && game.Exes[s.Name]
}

// matchPart reports whether one target part (never master/mic/other) covers the session.
func matchPart(part string, s SessionInfo, game *Game) bool {
	switch part {
	case TargetSystem:
		return s.System
	case TargetSteamGame:
		return !s.System && gameOwns(game, s)
	case TargetMaster, TargetMic, TargetOther:
		return false
	}
	if s.System {
		return false
	}
	if !strings.HasSuffix(part, ".exe") {
		part += ".exe"
	}
	return s.Name == part || path.Base(strings.ReplaceAll(s.Path, `\`, "/")) == part
}

// SessionsFor returns the indexes of sessions controlled by pot `idx` given all pot targets.
func SessionsFor(idx int, targets []string, sessions []SessionInfo, game *Game) []int {
	parts := targetParts(targets[idx])
	var out []int
	for si, s := range sessions {
		for _, part := range parts {
			hit := false
			if part == TargetOther {
				hit = !s.System && !claimedByOthers(idx, targets, s, game)
			} else {
				hit = matchPart(part, s, game)
			}
			if hit {
				out = append(out, si)
				break
			}
		}
	}
	return out
}

func claimedByOthers(idx int, targets []string, s SessionInfo, game *Game) bool {
	for i, t := range targets {
		if i == idx {
			continue
		}
		for _, part := range targetParts(t) {
			if part != TargetOther && matchPart(part, s, game) {
				return true
			}
		}
	}
	return false
}

func hasPart(target, part string) bool {
	for _, p := range targetParts(target) {
		if p == part {
			return true
		}
	}
	return false
}
