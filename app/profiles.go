package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProfileStore keeps several named configurations; one is active. The COM port is shared by all.
type ProfileStore struct {
	Active   string            `json:"active"`
	Order    []string          `json:"order"`
	Profiles map[string]Config `json:"profiles"`
}

func profilesPath(dir string) string { return filepath.Join(dir, "profiles.json") }

// LoadProfiles reads profiles.json, migrating an older single config.json into a "Default" profile.
func LoadProfiles(dir string) (*ProfileStore, error) {
	data, err := os.ReadFile(profilesPath(dir))
	if err == nil {
		var ps ProfileStore
		if jerr := json.Unmarshal(data, &ps); jerr == nil && len(ps.Profiles) > 0 {
			for name, c := range ps.Profiles {
				base := DefaultConfig()
				raw, _ := json.Marshal(c)
				json.Unmarshal(raw, &base)
				if n, nerr := base.Normalize(); nerr == nil {
					ps.Profiles[name] = n
				} else {
					ps.Profiles[name], _ = DefaultConfig().Normalize()
				}
			}
			ps.fixOrder()
			return &ps, nil
		}
		err = errors.New("profiles.json unreadable, starting fresh")
	} else if !errors.Is(err, os.ErrNotExist) {
		return defaultStore(), err
	} else {
		err = nil
	}
	c, cerr := LoadConfig(dir) // migrate config.json if present
	ps := defaultStore()
	ps.Profiles["Default"] = c
	if cerr != nil {
		err = cerr
	}
	return ps, err
}

func defaultStore() *ProfileStore {
	c, _ := DefaultConfig().Normalize()
	return &ProfileStore{Active: "Default", Order: []string{"Default"}, Profiles: map[string]Config{"Default": c}}
}

func (ps *ProfileStore) fixOrder() {
	seen := map[string]bool{}
	var order []string
	for _, n := range ps.Order {
		if _, ok := ps.Profiles[n]; ok && !seen[n] {
			order = append(order, n)
			seen[n] = true
		}
	}
	for n := range ps.Profiles {
		if !seen[n] {
			order = append(order, n)
		}
	}
	ps.Order = order
	if _, ok := ps.Profiles[ps.Active]; !ok {
		ps.Active = order[0]
	}
}

func (ps *ProfileStore) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(ps, "", "  ")
	tmp := profilesPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, profilesPath(dir))
}

func (ps *ProfileStore) Current() Config { return ps.Profiles[ps.Active] }

func (ps *ProfileStore) SetCurrent(c Config) { ps.Profiles[ps.Active] = c }

func cleanProfileName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || len([]rune(n)) > 32 {
		return "", errors.New("profile names must be 1-32 characters")
	}
	return n, nil
}

// Apply performs a profile action and returns the configuration that is now active.
func (ps *ProfileStore) Apply(action, name, newName string) (Config, error) {
	port := ps.Current().Port
	switch action {
	case "switch":
		if _, ok := ps.Profiles[name]; !ok {
			return ps.Current(), fmt.Errorf("no profile called %q", name)
		}
		ps.Active = name
	case "create", "duplicate":
		n, err := cleanProfileName(newName)
		if err != nil {
			return ps.Current(), err
		}
		if _, ok := ps.Profiles[n]; ok {
			return ps.Current(), fmt.Errorf("a profile called %q already exists", n)
		}
		c, _ := DefaultConfig().Normalize()
		if action == "duplicate" {
			c = ps.Current()
		}
		ps.Profiles[n] = c
		ps.Order = append(ps.Order, n)
		ps.Active = n
	case "rename":
		n, err := cleanProfileName(newName)
		if err != nil {
			return ps.Current(), err
		}
		if _, ok := ps.Profiles[name]; !ok {
			return ps.Current(), fmt.Errorf("no profile called %q", name)
		}
		if _, ok := ps.Profiles[n]; ok && n != name {
			return ps.Current(), fmt.Errorf("a profile called %q already exists", n)
		}
		c := ps.Profiles[name]
		delete(ps.Profiles, name)
		ps.Profiles[n] = c
		for i, o := range ps.Order {
			if o == name {
				ps.Order[i] = n
			}
		}
		if ps.Active == name {
			ps.Active = n
		}
	case "delete":
		if len(ps.Profiles) <= 1 {
			return ps.Current(), errors.New("the last profile cannot be deleted")
		}
		if _, ok := ps.Profiles[name]; !ok {
			return ps.Current(), fmt.Errorf("no profile called %q", name)
		}
		delete(ps.Profiles, name)
		ps.fixOrder()
	default:
		return ps.Current(), fmt.Errorf("unknown profile action %q", action)
	}
	c := ps.Current()
	c.Port = port // the COM port belongs to the PC, not the profile
	ps.SetCurrent(c)
	return c, nil
}
