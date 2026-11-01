package main

// WP1b/WP1d: provider output is bounded on disk during streaming
// and mirrored to the bound session's live stream — never an
// unbounded file, never invisible in the TUI.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A 1.5 MiB burst must land capped: the attempt file stays within
// the declared bound, the excess is drained (producer never
// blocks), and a truncation receipt names the loss.
func TestOutputCappedDuringStreaming(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "oc1")
	seedOwnedSession(t, "w", "oc1")
	tk, err := createTask("run-oc1", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	big := fakeWorkerAdapter("sh", "-c", "head -c 1500000 /dev/zero | tr '\\0' 'x'; echo END-MARKER")
	if ran, err := runWorkerOnce("w", big, testReq("op")); err != nil || !ran {
		t.Fatalf("loop must execute: ran=%v err=%v", ran, err)
	}
	work := loadRegistry()["w"].Workdir
	fi, err := os.Stat(filepath.Join(work, "worker-output-"+tk.ID+"-attempt1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > workerOutputCap {
		t.Fatalf("output file %d bytes exceeds cap %d", fi.Size(), workerOutputCap)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "output_truncated" && strings.Contains(r.Detail, "attempt=1") {
			found = true
		}
	}
	if !found {
		t.Fatal("truncation must leave a receipt naming the attempt")
	}
}

// The same burst must be visible in the session's live stream:
// the TUI reads that file first, so real execution shows where
// the operator looks.
func TestOutputMirroredToSessionStream(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "oc2")
	seedOwnedSession(t, "w", "oc2")
	tk, err := createTask("run-oc2", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	mark := fakeWorkerAdapter("sh", "-c", "echo STREAM-VISIBLE-MARKER-42")
	if ran, err := runWorkerOnce("w", mark, testReq("op")); err != nil || !ran {
		t.Fatalf("loop must execute: ran=%v err=%v", ran, err)
	}
	data, err := os.ReadFile(streamPath("w"))
	if err != nil {
		t.Fatalf("session stream must exist: %v", err)
	}
	if !strings.Contains(string(data), "STREAM-VISIBLE-MARKER-42") {
		t.Fatalf("provider output must reach the session stream, got %d bytes without the marker", len(data))
	}
	if !strings.Contains(string(data), tk.ID) {
		t.Fatal("stream mirror must name the task it belongs to")
	}
	if len(data) > workerStreamCap {
		t.Fatalf("stream mirror %d bytes exceeds its bound %d", len(data), workerStreamCap)
	}
}
