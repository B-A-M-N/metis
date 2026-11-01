package main

// resource_metrics.go: observe-only resource-usage snapshot (Metis
// audit remediation P3, observe-only).
//
// One snapshot function reports process and fleet pressure without
// changing anything: goroutine count, heap allocation, registered
// input-sender lane count, and per-session stream bytes against the
// retention cap (tmuxStreamCap, owned by session_stream.go /
// retention.go).
//
// What this is NOT:
//   - No new goroutines. The snapshot runs inline on the caller's
//     goroutine; nothing is spawned, no sweeper or ticker here.
//   - No CLI/MCP wiring. No commands or tools read this; tests and
//     future operators call the function directly.
//   - No persistence writes. No registry, generation, gap-marker,
//     or failure-marker writes — observation never rotates a
//     stream, resurrects a lane, or bumps a counter.
//   - Never blocks. One stat per registered session, one short
//     mutex hold for the lane count, no flock, no tmux forks, no
//     channel operations, no I/O beyond bounded registry/stream
//     stats. Control flow never branches on the result.

import (
	"os"
	"runtime"
	"sort"
)

// streamResourceUsage is one session's live-stream size against the
// retention cap. Bytes is -1 when the stream file is missing or
// unstatable (never proof of an empty stream, only that no size
// was observed); OverCap is then false.
type streamResourceUsage struct {
	Session string
	Bytes   int64
	OverCap bool
}

// resourceUsageSnapshot is one point-in-time pressure reading.
// It is a plain value: safe to keep, compare, or log, with no
// handles back into live state.
type resourceUsageSnapshot struct {
	Goroutines  int
	HeapAlloc   uint64
	SenderLanes int
	StreamCap   int64
	Streams     []streamResourceUsage
}

// snapshotResourceUsage takes the snapshot. Observe-only: it
// creates no lanes (liveInputSenderCount only counts), rotates no
// streams (os.Stat only reads sizes), writes no files, spawns no
// goroutines, and blocks on nothing held by another subsystem.
func snapshotResourceUsage() resourceUsageSnapshot {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	snap := resourceUsageSnapshot{
		Goroutines:  runtime.NumGoroutine(),
		HeapAlloc:   mem.HeapAlloc,
		SenderLanes: liveInputSenderCount(),
		StreamCap:   tmuxStreamCap,
	}
	for name := range loadRegistry() {
		u := streamResourceUsage{Session: name, Bytes: -1}
		if fi, err := os.Stat(streamPath(name)); err == nil {
			u.Bytes = fi.Size()
			u.OverCap = fi.Size() > tmuxStreamCap
		}
		snap.Streams = append(snap.Streams, u)
	}
	sort.Slice(snap.Streams, func(i, j int) bool { return snap.Streams[i].Session < snap.Streams[j].Session })
	return snap
}
