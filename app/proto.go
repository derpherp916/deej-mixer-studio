package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ---------- Nano serial protocol ----------

const helloLine = "HELLO,DEEJMIXER,3"

// helloPrefix matches any Deej Mixer firmware version (older ones need updating).
const helloPrefix = "HELLO,DEEJMIXER,"

type MsgKind int

const (
	MsgNone MsgKind = iota
	MsgHello
	MsgFrame
	MsgPress
	MsgRelease
)

type Msg struct {
	Kind   MsgKind
	Values [5]int
	Mask   int
	Button int
}

func ParseLine(line string) Msg {
	line = strings.TrimSpace(line)
	if line == helloLine {
		return Msg{Kind: MsgHello}
	}
	parts := strings.Split(line, ",")
	switch {
	case len(parts) == 7 && parts[0] == "V":
		var m Msg
		m.Kind = MsgFrame
		for i := 0; i < 6; i++ {
			n, err := strconv.Atoi(parts[i+1])
			if err != nil {
				return Msg{}
			}
			if i < 5 {
				if n < 0 || n > 1023 {
					return Msg{}
				}
				m.Values[i] = n
			} else {
				if n < 0 || n > 63 {
					return Msg{}
				}
				m.Mask = n
			}
		}
		return m
	case len(parts) == 2 && (parts[0] == "P" || parts[0] == "R"):
		n, err := strconv.Atoi(parts[1])
		if err != nil || n < 0 || n > 5 {
			return Msg{}
		}
		k := MsgPress
		if parts[0] == "R" {
			k = MsgRelease
		}
		return Msg{Kind: k, Button: n}
	}
	return Msg{}
}

// Command adds the checksum the firmware requires: body*HH\n
func Command(body string) []byte {
	var sum byte
	for i := 0; i < len(body); i++ {
		sum ^= body[i]
	}
	return []byte(fmt.Sprintf("%s*%02X\n", body, sum))
}

// LineSplitter turns a byte stream into lines, guarding against runaway input.
type LineSplitter struct{ buf []byte }

func (s *LineSplitter) Feed(data []byte, emit func(string)) {
	for _, b := range data {
		switch b {
		case '\n':
			emit(string(s.buf))
			s.buf = s.buf[:0]
		case '\r':
		default:
			if len(s.buf) < 256 {
				s.buf = append(s.buf, b)
			}
		}
	}
}

// ---------- hotkeys ----------

var vkNames = map[string]uint16{
	"CTRL": 0x11, "CONTROL": 0x11, "SHIFT": 0x10, "ALT": 0x12, "WIN": 0x5B,
	"SPACE": 0x20, "ENTER": 0x0D, "TAB": 0x09, "ESC": 0x1B, "ESCAPE": 0x1B, "BACKSPACE": 0x08,
	"DELETE": 0x2E, "INSERT": 0x2D, "HOME": 0x24, "END": 0x23, "PAGEUP": 0x21, "PAGEDOWN": 0x22,
	"LEFT": 0x25, "UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28, "PRINTSCREEN": 0x2C, "PAUSE": 0x13,
	"VOLUMEUP": 0xAF, "VOLUMEDOWN": 0xAE, "VOLUMEMUTE": 0xAD,
	"PLAYPAUSE": 0xB3, "NEXTTRACK": 0xB0, "PREVTRACK": 0xB1, "STOP": 0xB2,
}

func init() {
	for i := 1; i <= 24; i++ {
		vkNames[fmt.Sprintf("F%d", i)] = uint16(0x6F + i)
	}
	for c := 'A'; c <= 'Z'; c++ {
		vkNames[string(c)] = uint16(c)
	}
	for c := '0'; c <= '9'; c++ {
		vkNames[string(c)] = uint16(c)
	}
}

var errHotkey = errors.New("hotkey must be keys joined by +, e.g. CTRL+SHIFT+M, F13, ALT+TAB, WIN+D")

func ParseHotkey(s string) ([]uint16, error) {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if s == "" {
		return nil, errHotkey
	}
	var out []uint16
	seen := map[uint16]bool{}
	for _, part := range strings.Split(s, "+") {
		vk, ok := vkNames[part]
		if !ok {
			return nil, fmt.Errorf("unknown key %q. %v", part, errHotkey)
		}
		if seen[vk] {
			return nil, errors.New("a key is repeated in the hotkey")
		}
		seen[vk] = true
		out = append(out, vk)
	}
	return out, nil
}
