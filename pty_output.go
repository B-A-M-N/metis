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
	case "up":
		return "\x1b[A", true
	case "down":
		return "\x1b[B", true
	case "right":
		return "\x1b[C", true
	case "left":
		return "\x1b[D", true
	case "ctrl+c":
		return "\x03", true
	case "esc", "pgup", "pgdn":
		return "", false
	default:
		if len(runes) > 0 {
			return string(runes), true
		}
		return "", false
	}
}

// ptyMark updates status/exit for a session row.
// ptyAbandon marks a pre-claim row gone only while it still belongs
// to this startup's owner. A replacement that took the row meanwhile
// is left untouched; its own supervisor owns the record now.
