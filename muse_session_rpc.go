package main

// muse_session_rpc.go: native session lifecycle over a museHost —
// create (idempotent), read, resume, list, and config checks.
//
// Admission is never completion: session/start returns the created
// session identity and the starting view cursor; turn outcomes
// arrive later as turn/completed notifications (see
// muse_observation.go). Callers must wait for that event before
// recording any execution result.
//
// Identity rules mirrored from the wire (verified on 1.4.14):
//   - commandId is client-minted UUIDv7 and is the idempotency
//     handle: a retried start with the same commandId is
//     deduplicated by the server to the same result.
//   - view cursors are opaque server-minted strings (`v4:<sid>:<n>`
//     today); never parse, construct, or compare them — only store
//     and replay.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// newMuseCommandID mints a UUIDv7 idempotency handle: 48-bit unix-ms
// timestamp, version nibble 7, RFC 4122 variant. The server refuses
// non-v7 commandIds before any durable byte, so this shape is load
// bearing, not cosmetic.
func newMuseCommandID() (string, error) {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	if _, err := rand.Read(b[6:16]); err != nil {
		return "", fmt.Errorf("muse command id entropy: %w", err)
	}
	b[6] = (b[6] & 0x0F) | 0x70
	b[8] = (b[8] & 0x3F) | 0x80
	h := fmt.Sprintf("%x", b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// museStartSession creates a brand-new native session and returns its
// server-minted session ID plus the starting view cursor. The cursor
// may be empty when the session has no events yet; an empty cursor is
// a valid "from now" position, not an error. workspaceRoot, when
// non-empty, is passed as the session's authorized root so the
// server never inherits an ambient working directory by accident.
//
// The command identity is persisted before transmitting
// session/start, exactly like turn intents: an acknowledgment loss
// leaves the intent submitted, and a retry with the same caller key
// returns the already-created session instead of minting another.
func museStartSession(ctx context.Context, h *museHost, title, workspaceRoot string) (sessionID, cursor string, err error) {
	cmdID, err := newMuseCommandID()
	if err != nil {
		return "", "", err
	}
	return museStartSessionWithID(ctx, h, title, workspaceRoot, museSessionSelection{}, cmdID)
}

// museSessionSelection is the authorized provider/model routing for
// one native session. Empty selects the server default; a non-empty
// value must be confirmed back by the server, never assumed.
type museSessionSelection struct {
	ProviderID string
	ModelID    string
}

// museReadSessionConfig returns the server-reported effective
// provider, model, and workspace root for one native session.
// Empty means the server omitted it — never a confirmed value.
func museReadSessionConfig(ctx context.Context, h *museHost, sessionID string) (provider, model, workspaceRoot string, err error) {
	raw, err := h.Call(ctx, "session/read", map[string]any{"sessionId": sessionID})
	if err != nil {
		return "", "", "", fmt.Errorf("session/read: %w", err)
	}
	var res struct {
		Session struct {
			ProviderID    string `json:"providerId"`
			ModelID       string `json:"modelId"`
			WorkspaceRoot string `json:"workspaceRoot"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", "", fmt.Errorf("session/read result: %w", err)
	}
	return res.Session.ProviderID, res.Session.ModelID, res.Session.WorkspaceRoot, nil
}

// museVerifySessionConfig blocks admission when the server-reported
// effective config contradicts the authorized selection: a
// requested provider/model/workspace the server did not load is
// never silently accepted. Unrequested dimensions (empty want)
// pass through unverified and are returned as reported.
func museVerifySessionConfig(ctx context.Context, h *museHost, sessionID, wantProvider, wantModel, wantWorkspaceRoot string) (effProvider, effModel, effWorkspace string, err error) {
	prov, model, ws, err := museReadSessionConfig(ctx, h, sessionID)
	if err != nil {
		return "", "", "", err
	}
	if wantProvider != "" && prov != wantProvider {
		return "", "", "", fmt.Errorf("muse session config mismatch: authorized provider %q, server reports %q; refusing to adopt the session", wantProvider, prov)
	}
	if wantModel != "" && model != wantModel {
		return "", "", "", fmt.Errorf("muse session config mismatch: authorized model %q, server reports %q; refusing to adopt the session", wantModel, model)
	}
	if wantWorkspaceRoot != "" && filepath.Clean(ws) != filepath.Clean(wantWorkspaceRoot) {
		return "", "", "", fmt.Errorf("muse session config mismatch: authorized workspace %q, server reports %q; refusing to adopt the session", wantWorkspaceRoot, ws)
	}
	return prov, model, ws, nil
}

// museStartSessionWithID is the idempotent session creation path:
// the caller owns the command identity, so a retry of the same
// operation reuses it and the server dedups instead of creating a
// second native session.
func museStartSessionWithID(ctx context.Context, h *museHost, title, workspaceRoot string, sel museSessionSelection, commandID string) (sessionID, cursor string, err error) {
	if strings.TrimSpace(commandID) == "" {
		return "", "", fmt.Errorf("session/start needs a caller command identity; refusing to mint inside the retry path")
	}
	// Durability lives in muse_session_store.go: the intent row
	// is created before transmitting, so an ack loss leaves a
	// submitted record and the retry below reuses it.
	intent, found, lerr := loadSessionIntent(commandID)
	if lerr != nil {
		return "", "", lerr
	}
	if !found {
		if cerr := createSessionIntent(commandID); cerr != nil {
			return "", "", cerr
		}
	} else if intent.State == museSessionIntentAcknowledged && intent.NativeID != "" {
		// Already created: return the durable result, send nothing.
		return intent.NativeID, intent.ViewCursor, nil
	}
	// A submitted intent — first attempt, or a retry after an ack
	// loss — re-sends the SAME command identity: the server
	// deduplicates it to the original session instead of creating
	// a second one. Never mint a fresh command on this path.
	params := map[string]any{"commandId": commandID, "title": title}
	if strings.TrimSpace(workspaceRoot) != "" {
		params["workspaceRoot"] = workspaceRoot
	}
	if strings.TrimSpace(sel.ProviderID) != "" {
		params["providerId"] = sel.ProviderID
	}
	if strings.TrimSpace(sel.ModelID) != "" {
		params["modelId"] = sel.ModelID
	}
	raw, err := h.Call(ctx, "session/start", params)
	if err != nil {
		return "", "", fmt.Errorf("session/start: %w", err)
	}
	var res struct {
		Session struct {
			SessionID string `json:"sessionId"`
		} `json:"session"`
		ViewCursor string `json:"viewCursor"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", fmt.Errorf("session/start result: %w", err)
	}
	if res.Session.SessionID == "" {
		return "", "", fmt.Errorf("session/start returned no session id")
	}
	// The local selection is not proof of what Muse loaded: read
	// back the served Session and refuse a session whose effective
	// provider, model, or workspace contradicts the authorized
	// selection. Nothing is bound on mismatch.
	if _, _, _, verr := museVerifySessionConfig(ctx, h, res.Session.SessionID, sel.ProviderID, sel.ModelID, workspaceRoot); verr != nil {
		return "", "", verr
	}
	// Acknowledge the intent only after the server confirms: an
	// ack loss leaves the row submitted, and the retry reuses
	// the same command id instead of creating a second session.
	// Best-effort by design (unchanged): the returned identity is
	// already correct, and failing here would push the caller to
	// mint a FRESH command — a second session — so the write error
	// is absorbed, never propagated.
	_ = acknowledgeSessionIntent(commandID, res.Session.SessionID, res.ViewCursor)
	return res.Session.SessionID, res.ViewCursor, nil
}

// museResumeSession loads a stored native session onto this host
// and auto-subscribes the connection. cursor is an opaque view
// cursor previously observed by THIS client; omitted means "serve
// what a resume without a cursor would". A resume that loads the
// session writes a durable SessionResumed record. Returns the
// session's current status and the resume view cursor.
func museResumeSession(ctx context.Context, h *museHost, sessionID, cursor string) (status, viewCursor string, err error) {
	cmdID, err := newMuseCommandID()
	if err != nil {
		return "", "", err
	}
	params := map[string]any{"commandId": cmdID, "sessionId": sessionID}
	if cursor != "" {
		params["cursor"] = cursor
	}
	raw, err := h.Call(ctx, "session/resume", params)
	if err != nil {
		return "", "", fmt.Errorf("session/resume: %w", err)
	}
	var res struct {
		Session struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		} `json:"session"`
		ViewCursor string `json:"viewCursor"`
		History    struct {
			Mode string `json:"mode"`
		} `json:"history"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", fmt.Errorf("session/resume result: %w", err)
	}
	if res.Session.SessionID == "" {
		return "", "", fmt.Errorf("session/resume returned no session id")
	}
	return res.Session.Status, res.ViewCursor, nil
}

// museReadSession reads one stored session without attaching: no
// writer lease, no load, no subscription, no SessionResumed record.
// Point-in-time only — immediately stale if a foreign host appends.
func museReadSession(ctx context.Context, h *museHost, sessionID string) (status, viewCursor string, err error) {
	raw, err := h.Call(ctx, "session/read", map[string]any{"sessionId": sessionID})
	if err != nil {
		return "", "", fmt.Errorf("session/read: %w", err)
	}
	var res struct {
		Session struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		} `json:"session"`
		ViewCursor string `json:"viewCursor"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", fmt.Errorf("session/read result: %w", err)
	}
	return res.Session.Status, res.ViewCursor, nil
}

// museSessionList pages stored sessions. Read-only; never touches
// leases. Returns session IDs oldest-first per page with the next
// page cursor ("" when exhausted).
func museSessionList(ctx context.Context, h *museHost, limit int, cursor string) (ids []string, next string, err error) {
	params := map[string]any{}
	if limit > 0 {
		params["limit"] = limit
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	raw, err := h.Call(ctx, "session/list", params)
	if err != nil {
		return nil, "", fmt.Errorf("session/list: %w", err)
	}
	var res struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
		NextCursor string `json:"nextCursor"`
		Cursor     string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, "", fmt.Errorf("session/list result: %w", err)
	}
	for _, s := range res.Sessions {
		if s.SessionID != "" {
			ids = append(ids, s.SessionID)
		}
	}
	if res.NextCursor != "" {
		next = res.NextCursor
	} else {
		next = res.Cursor
	}
	return ids, next, nil
}
