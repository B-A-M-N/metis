package main

// muse_host.go: supervision and JSON-RPC framing for one `muse serve`
// child. This file owns exactly one OS process, its pipes, and the
// request/reply correlation. It knows nothing about tasks, attempts,
// or verification — session semantics live in muse_sessions.go.
//
// The pure wire vocabulary (frame encode/decode, error and
// notification types, view-cursor extraction) lives in
// internal/musewire; this file keeps process supervision, the pending
// correlation map, and the public host API. Wire facts (installed
// Muse 1.4.14, schema fingerprint recorded at handshake; verified
// against the echo provider, zero inference):
//   - initialize + `initialized` notification opens the connection;
//     anything else is refused `notInitialized` (-32600).
//   - Replies and notifications interleave on stdout; correlation is
//     by numeric id. Notification-only traffic gets no reply.
//   - Closing stdin initiates orderly host shutdown.
//   - commandId must be client-minted UUIDv7; the server rejects
//     anything else before any durable byte.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"tui-dash/internal/musewire"
)

const (
	// metisMuseClientName is the MSP client identification. It is
	// diagnostics only, never an authority claim.
	metisMuseClientName    = musewire.ClientName
	metisMuseClientVersion = musewire.ClientVersion
	// museCallTimeout bounds one request/reply exchange.
	museCallTimeout = musewire.CallTimeout
	// museMaxLine bounds one JSON-RPC frame. View events can be
	// large; overflow is a loud error, never a silent split.
	museMaxLine = musewire.MaxLineBytes
)

// museError carries a JSON-RPC error with the server's kind label
// when present, so callers can distinguish retryable overload
// (-32001) from terminal refusal without string matching.
type museError = musewire.Error

// museNotification is one server-initiated MSP message with its raw
// params: a notification, or a must-answer request
// (`approval/request`, `userInput/request`). RequestID is non-nil
// exactly for must-answer requests; the presentation receipt (and
// any decision) travels separately, so surfacing never implies
// answering. View cursors ride inside method-specific params; use
// museViewCursor to extract the cursor when the method carries one.
type museNotification = musewire.Notification

// museViewCursor returns the view cursor from notification params
// when present, or "" when the method carries none.
func museViewCursor(n museNotification) string {
	return musewire.ViewCursor(musewire.Notification(n))
}

type musePending struct {
	ch chan museResult
}

type museResult struct {
	result json.RawMessage
	err    *museError
}

// museHost is one supervised `muse serve` child plus its framing.
// Exactly one host per process pair: stdio mode serves a single
// connection, so a second client must spawn its own host.
type museHost struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[int64]chan museResult
	nextID    atomic.Int64

	notifCh chan museNotification
	dropped atomic.Uint64

	doneCh  chan struct{}
	doneErr atomic.Value // error

	// Negotiated at handshake; recorded, not trusted.
	ServerVersion string
	Fingerprint   string
	SchemaVersion int
	Capabilities  []string
}

// newMuseHostIO runs the MSP handshake over an already-open
// transport. Production passes a child process's pipes; tests pass
// an in-memory pipe pair against a canned server.
func newMuseHostIO(r io.Reader, w io.WriteCloser) (*museHost, error) {
	h := &museHost{
		stdin:   w,
		pending: map[int64]chan museResult{},
		notifCh: make(chan museNotification, 1024),
		doneCh:  make(chan struct{}),
	}
	h.nextID.Store(1)
	go h.readLoop(r)
	var initRes struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Schema struct {
			Fingerprint string `json:"fingerprint"`
			Version     int    `json:"version"`
		} `json:"schema"`
		GrantedCapabilities []string `json:"grantedCapabilities"`
	}
	raw, err := h.call(context.Background(), "initialize", map[string]any{
		"clientInfo": map[string]string{"name": metisMuseClientName, "version": metisMuseClientVersion},
	})
	if err != nil {
		h.abort()
		return nil, fmt.Errorf("muse initialize: %w", err)
	}
	if err := json.Unmarshal(raw, &initRes); err != nil {
		h.abort()
		return nil, fmt.Errorf("muse initialize result: %w", err)
	}
	h.ServerVersion = initRes.ServerInfo.Version
	h.Fingerprint = initRes.Schema.Fingerprint
	h.SchemaVersion = initRes.Schema.Version
	h.Capabilities = initRes.GrantedCapabilities
	if err := h.notify("initialized", map[string]any{}); err != nil {
		h.abort()
		return nil, fmt.Errorf("muse initialized: %w", err)
	}
	return h, nil
}

// museHomeDir resolves the muse store root the same way the daemon
// does: $XDG_DATA_HOME or ~/.local/share. The well-known session
// socket lives beneath it; all --join hosts in this process tree
// must agree on it or they land in different stores.
func museHomeDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return xdg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.local/share"
}

// museDaemonSocket is the well-known session-server socket for the
// resolved muse home. Daemon and joiners rendezvous here; its
// absence means no daemon serves this home.
func museDaemonSocket() string {
	if h := museHomeDir(); h != "" {
		return h + "/muse/session-server/session-server.sock"
	}
	return ""
}

// museDaemonReady reports whether a session daemon serves this home.
// Metis refuses to proceed without one: a bare `muse serve`
// invocation would serve in-process and its sessions would die with
// the call, silently breaking cross-invocation resume.
func museDaemonReady() bool {
	sock := museDaemonSocket()
	if sock == "" {
		return false
	}
	fi, err := os.Stat(sock)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

// startMuseHost spawns `<binary> serve --provider <provider>` with
// the given extra argv and environment, then handshakes. binary is
// the resolved commander binary ("muse", or "musefi" for the
// separate FI-profiled launcher): never a caller-supplied path, so
// only allowlisted commanders can pick the executable. env entries
// override the inherited process environment (last value wins), so
// callers pass only deltas like XDG_DATA_HOME to isolate the session
// store (tests do; production uses the commander's home deliberately).
// Unlike a remotely-selected child, the host binary is the
// operator's own trusted provider: it keeps a functioning ambient
// environment (PATH, HOME, auth) rather than a scrubbed one.
//
// Pass "--join" in extraArgs to attach through the session daemon
// (required for any session that must outlive this call). Callers
// that need cross-invocation durability must check the commander's
// daemon socket first and refuse when no daemon serves that store.
func startMuseHost(ctx context.Context, binary, provider string, env []string, extraArgs ...string) (*museHost, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("muse binary %q not on PATH: %w", binary, err)
	}
	argv := append([]string{"serve", "--provider", provider}, extraArgs...)
	cmd := exec.CommandContext(ctx, path, argv...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("muse stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("muse stdout pipe: %w", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("muse serve start: %w", err)
	}
	h, err := newMuseHostIO(stdout, stdin)
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, err
	}
	h.cmd = cmd
	return h, nil
}

// call performs one request/reply exchange, correlating by id across
// interleaved notifications. A notification-only message never
// satisfies a call.
func (h *museHost) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := h.nextID.Add(1)
	body, err := musewire.EncodeCall(id, method, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan museResult, 1)
	h.pendingMu.Lock()
	h.pending[id] = ch
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, id)
		h.pendingMu.Unlock()
	}()
	h.writeMu.Lock()
	_, werr := h.stdin.Write(body)
	h.writeMu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("muse write %s: %w", method, werr)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, museCallTimeout)
		defer cancel()
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("muse %s: %w", method, ctx.Err())
	case <-h.doneCh:
		if v := h.doneErr.Load(); v != nil {
			if derr, ok := v.(error); ok && derr != nil {
				return nil, derr
			}
		}
		return nil, fmt.Errorf("muse %s: host closed", method)
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.result, nil
	}
}

// Call is the public request/reply exchange.
func (h *museHost) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return h.call(ctx, method, params)
}

// notify sends a notification: no reply ever comes back.
func (h *museHost) notify(method string, params any) error {
	body, err := musewire.EncodeNotify(method, params)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err = h.stdin.Write(body)
	return err
}

// Notifications exposes the server-initiated stream. The channel is
// buffered; overflow drops oldest-first and counts via Dropped.
func (h *museHost) Notifications() <-chan museNotification { return h.notifCh }

// Dropped counts notifications discarded under overflow.
func (h *museHost) Dropped() uint64 { return h.dropped.Load() }

func (h *museHost) pushNotif(n museNotification) {
	select {
	case h.notifCh <- n:
	default:
		select {
		case <-h.notifCh:
			h.dropped.Add(1)
		default:
			h.dropped.Add(1)
		}
		select {
		case h.notifCh <- n:
		default:
			h.dropped.Add(1)
		}
	}
}

func (h *museHost) readLoop(r io.Reader) {
	// Closing notifCh unblocks museWaitTurn with a closed-host
	// error instead of a hang: readLoop is the sole sender on
	// that channel (via pushNotif), so no send can follow.
	defer close(h.doneCh)
	defer close(h.notifCh)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), museMaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		fr, ok := musewire.DecodeFrame(line)
		if !ok {
			continue // untrusted framing; a bad line is not a state change
		}
		if fr.IsServerMessage() {
			// Server-initiated traffic always surfaces: a
			// notification, or a must-answer request (id +
			// method, e.g. approval/request). Replies never
			// carry a method, so this branch cannot steal a
			// pending call's reply. Unanswered requests stay
			// pending server-side and are re-issued on the next
			// subscribe — surfacing is not answering.
			var rid *int64
			if fr.ID != nil {
				id := *fr.ID
				rid = &id
			}
			h.pushNotif(museNotification{Method: fr.Method, Params: fr.Params, RequestID: rid})
			continue
		}
		if fr.IsReply() {
			h.pendingMu.Lock()
			ch, ok := h.pending[*fr.ID]
			h.pendingMu.Unlock()
			if !ok {
				continue // late or foreign reply; no waiter owns it
			}
			if fr.Err != nil {
				ch <- museResult{err: &museError{Code: fr.Err.Code, Kind: fr.Err.Kind, Message: fr.Err.Message}}
			} else {
				ch <- museResult{result: fr.Result}
			}
			continue
		}
		// Anything left has neither id nor method: not a reply,
		// not server traffic. Ignore it, same as a bad line.
	}
	if err := sc.Err(); err != nil {
		h.doneErr.Store(fmt.Errorf("muse host read: %w", err))
	}
}

// sendResult answers one server-initiated request (a presentation
// receipt or similar). It is a bare result frame, not a call: no
// waiter is registered and no reply is correlated.
func (h *museHost) sendResult(id int64, result any) error {
	body, err := musewire.EncodeResult(id, result)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	_, werr := h.stdin.Write(body)
	h.writeMu.Unlock()
	if werr != nil {
		return fmt.Errorf("muse write result %d: %w", id, werr)
	}
	return nil
}

// abort tears down a host that failed its handshake.
func (h *museHost) abort() {
	_ = h.stdin.Close()
	<-h.doneCh
	if h.cmd != nil {
		_ = h.cmd.Wait()
	}
}

// Close initiates orderly shutdown (stdin close), waits briefly,
// then kills a lingering child. Test transports have no process.
func (h *museHost) Close() error {
	_ = h.stdin.Close()
	if h.cmd == nil {
		<-h.doneCh
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-h.doneCh:
		return <-done
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
		return fmt.Errorf("muse host did not exit on stdin close; killed")
	}
}
