package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Button actions.
const (
	ActNone      = "none"
	ActMute      = "mute"     // value = pot channel "0".."4"
	ActMuteMic   = "mute_mic" // default microphone
	ActLaunch    = "launch"   // value = file, folder, URL or URI (steam://...)
	ActHotkey    = "hotkey"   // value = e.g. CTRL+SHIFT+M
	ActPlayPause = "play_pause"
	ActNext      = "next_track"
	ActPrev      = "prev_track"
)

var allActions = []string{ActMute, ActMuteMic, ActLaunch, ActHotkey, ActPlayPause, ActNext, ActPrev, ActNone}

type PotConfig struct {
	Label   string `json:"label"`
	Target  string `json:"target"`
	Reverse bool   `json:"reverse"`
}

type ButtonConfig struct {
	Action     string `json:"action"`
	Value      string `json:"value"`
	HoldAction string `json:"holdAction"`
	HoldValue  string `json:"holdValue"`
}

type Config struct {
	Version      int             `json:"version"`
	Port         string          `json:"port"` // "auto" or "COM5"
	Pots         [5]PotConfig    `json:"pots"`
	Buttons      [6]ButtonConfig `json:"buttons"`
	LogoChannel  [4]int          `json:"logoChannel"` // which pot each logo LED reflects
	LogoColors   [4]string       `json:"logoColors"`
	RingColor    string          `json:"ringColor"`
	MicColor     string          `json:"micColor"`
	MicIndicator bool            `json:"micIndicator"` // ring turns MicColor while the mic is muted
	Brightness   int             `json:"brightness"`   // ring brightness, percent 0..100
	IdleLogo     string          `json:"idleLogo"`     // logo LED when its app has no audio: "off", "dim", "on"
	ExtraGames   string          `json:"extraGames"`   // comma separated exe names treated as games
	HoldMs       int             `json:"holdMs"`
}

// Physical labels, index = button 0..5 = D2..D7.
var ButtonNames = [6]string{"Opera logo", "Steam logo", "Discord logo", "Spotify logo", "Small left", "Small right"}
var ButtonPins = [6]string{"D2", "D3", "D4", "D5", "D6", "D7"}

func DefaultConfig() Config {
	return Config{
		Version: 2,
		Port:    "auto",
		Pots: [5]PotConfig{
			{Label: "Master", Target: "master"},
			{Label: "Opera", Target: "opera.exe"},
			{Label: "Steam / current game", Target: "steam_game"},
			{Label: "Discord", Target: "discord.exe"},
			{Label: "Spotify", Target: "spotify.exe"},
		},
		Buttons: [6]ButtonConfig{
			{Action: ActMute, Value: "1", HoldAction: ActNone},
			{Action: ActMute, Value: "2", HoldAction: ActLaunch, HoldValue: "steam://open/main"},
			{Action: ActMute, Value: "3", HoldAction: ActLaunch, HoldValue: "discord://"},
			{Action: ActMute, Value: "4", HoldAction: ActLaunch, HoldValue: "spotify:"},
			{Action: ActMute, Value: "0", HoldAction: ActNone},
			{Action: ActMuteMic, HoldAction: ActNone},
		},
		LogoChannel:  [4]int{1, 2, 3, 4},
		LogoColors:   [4]string{"#FF1B2D", "#66C0F4", "#5865F2", "#1DB954"},
		RingColor:    "#30A0FF",
		MicColor:     "#FF2000",
		MicIndicator: true,
		Brightness:   60,
		IdleLogo:     "on",
		HoldMs:       600,
	}
}

var colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
var portRe = regexp.MustCompile(`^(auto|COM[0-9]{1,3})$`)

// Normalize fills blanks and validates. It returns a cleaned copy.
func (c Config) Normalize() (Config, error) {
	d := DefaultConfig()
	c.Version = 2
	c.Port = strings.ToUpper(strings.TrimSpace(c.Port))
	if c.Port == "" || c.Port == "AUTO" {
		c.Port = "auto"
	}
	if !portRe.MatchString(c.Port) {
		return c, fmt.Errorf("port must be auto or COMn, got %q", c.Port)
	}
	for i := range c.Pots {
		p := &c.Pots[i]
		p.Label = strings.TrimSpace(p.Label)
		if p.Label == "" {
			p.Label = fmt.Sprintf("Knob A%d", i)
		}
		p.Target = normalizeTarget(p.Target)
		if p.Target == "" {
			return c, fmt.Errorf("knob A%d needs an audio target", i)
		}
	}
	for i := range c.Buttons {
		b := &c.Buttons[i]
		if b.Action == "" {
			b.Action = ActNone
		}
		if b.HoldAction == "" {
			b.HoldAction = ActNone
		}
		var err error
		if b.Value, err = validateAction(b.Action, b.Value); err != nil {
			return c, fmt.Errorf("%s (%s) press: %v", ButtonNames[i], ButtonPins[i], err)
		}
		if b.HoldValue, err = validateAction(b.HoldAction, b.HoldValue); err != nil {
			return c, fmt.Errorf("%s (%s) hold: %v", ButtonNames[i], ButtonPins[i], err)
		}
	}
	for i, ch := range c.LogoChannel {
		if ch < 0 || ch > 4 {
			return c, fmt.Errorf("logo LED %d must follow knob 0-4", i+1)
		}
	}
	for i := range c.LogoColors {
		if c.LogoColors[i] == "" {
			c.LogoColors[i] = d.LogoColors[i]
		}
		if !colorRe.MatchString(c.LogoColors[i]) {
			return c, fmt.Errorf("logo LED %d colour must look like #RRGGBB", i+1)
		}
		c.LogoColors[i] = strings.ToUpper(c.LogoColors[i])
	}
	for _, col := range []*string{&c.RingColor, &c.MicColor} {
		if *col == "" {
			*col = d.RingColor
		}
		if !colorRe.MatchString(*col) {
			return c, errors.New("ring colours must look like #RRGGBB")
		}
		*col = strings.ToUpper(*col)
	}
	if c.Brightness < 0 || c.Brightness > 100 {
		return c, errors.New("brightness must be 0-100%")
	}
	switch c.IdleLogo {
	case "off", "dim", "on":
	case "":
		c.IdleLogo = "on"
	default:
		return c, errors.New("idle logo mode must be off, dim or on")
	}
	if c.HoldMs == 0 {
		c.HoldMs = d.HoldMs
	}
	if c.HoldMs < 250 || c.HoldMs > 3000 {
		return c, errors.New("hold time must be 250-3000 ms")
	}
	var games []string
	for _, g := range strings.Split(c.ExtraGames, ",") {
		if g = strings.ToLower(strings.TrimSpace(g)); g != "" {
			games = append(games, g)
		}
	}
	c.ExtraGames = strings.Join(games, ", ")
	return c, nil
}

func normalizeTarget(t string) string {
	var parts []string
	for _, p := range strings.Split(t, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "steam_recent" {
			p = TargetSteamGame
		}
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

func validateAction(action, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch action {
	case ActNone, ActMuteMic, ActPlayPause, ActNext, ActPrev:
		return "", nil
	case ActMute:
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 4 {
			return value, errors.New("mute needs a knob number 0-4")
		}
		return strconv.Itoa(n), nil
	case ActLaunch:
		if value == "" {
			return value, errors.New("launch needs a program path or link")
		}
		return value, nil
	case ActHotkey:
		if _, err := ParseHotkey(value); err != nil {
			return value, err
		}
		return strings.ToUpper(strings.ReplaceAll(value, " ", "")), nil
	}
	return value, fmt.Errorf("unknown action %q", action)
}

func (c Config) ExtraGameList() []string {
	var out []string
	for _, g := range strings.Split(c.ExtraGames, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// ---------- persistence ----------

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

func LoadConfig(dir string) (Config, error) {
	data, err := os.ReadFile(configPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return DefaultConfig(), err
	}
	c := DefaultConfig()
	if err := json.Unmarshal(data, &c); err != nil {
		return DefaultConfig(), fmt.Errorf("config.json unreadable, using defaults: %w", err)
	}
	n, err := c.Normalize()
	if err != nil {
		return DefaultConfig(), fmt.Errorf("config.json invalid, using defaults: %w", err)
	}
	return n, nil
}

func SaveConfig(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(dir))
}

func parseColor(s string) (r, g, b uint8) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return uint8(v >> 16), uint8(v >> 8), uint8(v)
}
