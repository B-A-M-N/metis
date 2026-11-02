package main

import (
	"fmt"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// tui_info.go: bounded, read-only detail collection for the collapsed
// session drawer. Existing ops diagnostics and resource snapshots remain
// their authorities; this adapter only filters and presents them.

type sessionInfoMsg struct {
	display          string
	diagnostics      []opsEvent
	resources        resourceUsageSnapshot
	stream           streamResourceUsage
	streamFound      bool
	diagnosticSource string
}

func sessionInfoCmd(display, canonical string) tea.Cmd {
	return func() tea.Msg {
		resources := snapshotResourceUsage()
		var stream streamResourceUsage
		streamFound := false
		for _, usage := range resources.Streams {
			if usage.Session == canonical || usage.Session == display {
				stream, streamFound = usage, true
				break
			}
		}
		sourceEvents := opsSnapshot()
		sinkEvents, sinkErr := readOpsSink(opsSinkMaxLines)
		source := "process-local ring"
		if sinkErr == nil {
			source = "ring + persisted log"
		} else {
			source += " only; log unavailable"
		}
		byKey := make(map[string]opsEvent)
		for _, ev := range append(sourceEvents, sinkEvents...) {
			if ev.Session != canonical && ev.Session != display || ev.Disposition != opsDispositionError {
				continue
			}
			key := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s", ev.At.UTC().Format(time.RFC3339Nano),
				ev.Phase, ev.RequestID, ev.Attempt, ev.ErrorClass, ev.Detail)
			byKey[key] = ev
		}
		diagnostics := make([]opsEvent, 0, len(byKey))
		for _, ev := range byKey {
			diagnostics = append(diagnostics, ev)
		}
		sort.Slice(diagnostics, func(i, j int) bool {
			return diagnostics[i].At.After(diagnostics[j].At)
		})
		if len(diagnostics) > 4 {
			diagnostics = diagnostics[:4]
		}
		return sessionInfoMsg{display: display, diagnostics: diagnostics,
			resources: resources, stream: stream, streamFound: streamFound,
			diagnosticSource: source}
	}
}
