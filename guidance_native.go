package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// guidance_native.go: MSP steering for muse-native sessions.
//
// A native session has no terminal to type into and no guidance
// file its worker loop necessarily consumes: steering is a native
// turn on the bound session. This dispatcher submits exactly that
// — an authenticated, idempotent turn admitted by Muse — and
// settles the guidance row to the admission actually observed:
// delivered (with the server disposition + turn id), refused (no
// binding, scope mismatch), or failed (transport/daemon/submit).
// "Delivered" means Muse admitted the turn, never that the worker
// applied it; application is observed through subsequent turns.

// nativeGuidanceCommandID loads the submission identity for one
// guidance row, minting and persisting it on first use. Retried
// deliveries reuse the same id so the server dedups instead of
// running the instruction twice.
func nativeGuidanceCommandID(db *sql.DB, id string) (string, error) {
	var cmd string
	err := db.QueryRow(`SELECT command_id FROM guidance WHERE id = ?`, id).Scan(&cmd)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(cmd) != "" {
		return cmd, nil
	}
	cmd, err = newMuseCommandID()
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(`UPDATE guidance SET command_id = ?, updated = ? WHERE id = ?`,
		cmd, nowUTC(), id); err != nil {
		return "", err
	}
	return cmd, nil
}

// deliverNativeGuidance steers one bound native session through
// MSP and reports the admission outcome. It never touches the
// terminal/file path: for muse-native that path cannot steer.
func deliverNativeGuidance(id, session, text string, req OperationRequest) (string, error) {
	b, err := getMuseBinding(session)
	if err != nil {
		return "", fmt.Errorf("refused: no muse binding for session %s; bind it first", session)
	}
	if err := checkMuseExecutionScope(session, b); err != nil {
		return "", fmt.Errorf("refused: %v", err)
	}
	c, err := resolveMuseCommander(b.Profile)
	if err != nil {
		return "", err
	}
	if err := museRequireDaemonFor(c); err != nil {
		return "", err
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	cmdID, err := nativeGuidanceCommandID(db, id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, b.Provider, nil, "--join")
	if err != nil {
		return "", err
	}
	defer h.Close()
	if _, _, err := museResumeSession(ctx, h, b.NativeID, b.Cursor); err != nil {
		return "", fmt.Errorf("resume %s: %w", b.NativeID, err)
	}
	if _, _, _, verr := museVerifySessionConfig(ctx, h, b.NativeID, b.Provider, b.Model, b.Workspace); verr != nil {
		return "", verr
	}
	ack, err := museSubmitTurnWithID(ctx, h, b.NativeID, text, "queue", cmdID)
	if err != nil {
		return "", fmt.Errorf("steer turn: %w", err)
	}
	if ack.Status != "accepted" {
		return "", fmt.Errorf("steer turn not admitted: status=%q disposition=%q", ack.Status, ack.Disposition)
	}
	return fmt.Sprintf("admitted disposition=%s turn=%s cmd=%s (application observed through subsequent turns)",
		ack.Disposition, ack.TurnID, cmdID), nil
}
