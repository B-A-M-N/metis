package main

import (
	"fmt"
	"os"
	"strings"
)

// pty_output.go: extracted from terminal.go; behavior unchanged.

func ptyLog(name string, tailBytes int) ([]byte, error) {
	m, err := ptyGet(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(m.LogPath)
	if err != nil {
		return nil, fmt.Errorf("no output yet: %w", err)
	}
	if tailBytes > 0 && len(data) > tailBytes {
		data = data[len(data)-tailBytes:]
	}
	return data, nil
}

// ptyLogLines tails the session log as display lines.
func ptyLogLines(m ptyMeta, lines int) []string {
	data, err := os.ReadFile(m.LogPath)
	if err != nil || len(data) == 0 {
		if ptyWorkerAlive(m) {
			return []string{"(no output yet)"}
		}
		return []string{"(session not running)"}
	}
	if len(data) > 64<<10 {
		data = data[len(data)-(64<<10):]
	}
	ls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	if len(ls) == 0 {
		return []string{"(no output yet)"}
	}
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return ls
}

// ptyKeyBytes maps a Bubble Tea key press to PTY input bytes.
// ok=false means the key is handled locally (scroll/detach), not sent.
func ptyKeyBytes(key string, runes []rune) (string, bool) {
	switch key {
	case "enter":
		return "\r", true
	case "backspace":
		return "\x7f", true
	case "tab":
		return "\t", true
	case "shift+tab":
		return "\x1b[Z", true
	case "up":
		return "\x1b[A", true
	case "down":
		return "\x1b[B", true
	case "right":
		return "\x1b[C", true
	case "left":
		return "\x1b[D", true
	case "home":
		return "\x1b[H", true
	case "end":
		return "\x1b[F", true
	case "insert":
		return "\x1b[2~", true
	case "delete":
		return "\x1b[3~", true
	case "ctrl+c":
		return "\x03", true
	case "ctrl+d":
		return "\x04", true
	case "ctrl+a":
		return "\x01", true
	case "ctrl+e":
		return "\x05", true
	case "ctrl+k":
		return "\x0b", true
	case "ctrl+u":
		return "\x15", true
	case "ctrl+w":
		return "\x17", true
	case "ctrl+l":
		return "\x0c", true
	case "ctrl+z":
		return "\x1a", true
	case "ctrl+left":
		return "\x1b[1;5D", true
	case "ctrl+right":
		return "\x1b[1;5C", true
	case "ctrl+up":
		return "\x1b[1;5A", true
	case "ctrl+down":
		return "\x1b[1;5B", true
	case "esc", "pgup", "pgdn":
		return "", false
	default:
		// Alt+<rune> is ESC + rune (Meta); Bubble Tea names
		// it "alt+x" with the rune in Runes. Dropping the
		// prefix would deliver a plain rune instead.
		if strings.HasPrefix(key, "alt+") && len(runes) > 0 {
			return "\x1b" + string(runes), true
		}
		if len(runes) > 0 {
			return string(runes), true
		}
		if key == "space" {
			return " ", true
		}
		return "", false
	}
}

// ptyMark updates status/exit for a session row.
// ptyAbandon marks a pre-claim row gone only while it still belongs
// to this startup's owner. A replacement that took the row meanwhile
// is left untouched; its own supervisor owns the record now.
