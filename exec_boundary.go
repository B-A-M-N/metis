package main

// exec_boundary.go: execution security boundary for managed workers.
//
// A worker process must never inherit the controller's ambient
// authority by accident: not its environment credentials, not its
// bulk prompt text in receipts, not its files through symlinks.
// Three narrow helpers enforce that:
//
//   - workerEnv projects a closed allowlist plus explicit METIS_*
//     task bindings. Anything else — including *TOKEN*, *SECRET*,
//     *KEY* names — is dropped, never inherited.
//   - redactArgv/marshalRedactedArgv stores invocation structure
//     with bulk content and embedded secrets replaced by digests.
//   - openWorkerOutput refuses symlinks (a prior attempt's worker
//     can predict the next attempt's output path) while preserving
//     same-attempt retry truncation of owned regular files.
//   - readConfinedFile confines BEFORE reading and re-verifies the
//     opened fd, closing the check-then-read swap.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// workerEnvAllow lists the only ambient variables a managed worker
// may inherit. The FreeInference gateway is loopback, so no proxy or
// cloud-credential variables are included; extend deliberately, never
// by inheriting the whole environment.
var workerEnvAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE",
	"TERM", "TZ", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	"SHELL",
}

// secretEnvName drops any variable whose name looks like a credential,
// even if a future allowlist addition would otherwise admit it.
var secretEnvName = regexp.MustCompile(`(?i)^(.+_)?(api[_-]?key|secrets?|tokens?|passwds?|bearer|auth|credentials?|private[_-]?key)$`)

// workerEnv builds the child's environment: closed allowlist, secret
// names dropped, explicit METIS_* task bindings, then caller extras
// (e.g. METIS_STATE for a re-exec'd supervisor). Extras are the
// caller's responsibility; ambient values never leak in implicitly.
func workerEnv(taskID string, attempt int, session, root string, extra map[string]string) []string {
	var env []string
	for _, k := range workerEnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	env = append(env,
		"METIS_TASK_ID="+taskID,
		"METIS_ATTEMPT="+strconv.Itoa(attempt),
		"METIS_SESSION="+session,
		"METIS_EXECUTION_ROOT="+root,
	)
	out := env[:0]
	for _, e := range env {
		if i := strings.Index(e, "="); i > 0 && secretEnvName.MatchString(e[:i]) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// secretValue scrubs embedded key=value secrets from short args:
// api_key=..., token: ..., "password": "...".
var secretValue = regexp.MustCompile(`(?i)\b(api[_-]?key|secrets?|tokens?|passwds?|password|bearer)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;}"']+)`)

// maxStoredArg keeps invocation structure while refusing to persist
// bulk content (prompts, documents) in receipts.
const maxStoredArg = 256

// redactArgv replaces bulk args with size+digest markers and scrubs
// embedded secrets. Structure (argc, order, flags) is preserved for
// diagnostics; content is not.
func redactArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if len(a) > maxStoredArg {
			out[i] = fmt.Sprintf("<redacted %d bytes sha256:%x>", len(a), sha256.Sum256([]byte(a)))
			continue
		}
		out[i] = secretValue.ReplaceAllString(a, `$1=<redacted>`)
	}
	return out
}

// marshalRedactedArgv serializes the redacted invocation plus the
// digest of the full argv for correlation without content.
func marshalRedactedArgv(argv []string) (string, error) {
	full, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	red, err := json.Marshal(map[string]any{
		"argv":        redactArgv(argv),
		"redacted":    true,
		"full_sha256": fmt.Sprintf("%x", sha256.Sum256(full)),
	})
	if err != nil {
		return "", err
	}
	return string(red), nil
}

// openWorkerOutput opens the attempt log with symlink protection. A
// predicted path planted by an earlier attempt must be refused; a
// same-attempt retry (refundClaim keeps the attempt number) must
// still truncate the owned regular file it created.
func openWorkerOutput(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("worker output path %s is a symlink; refusing (possible plant)", path)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("worker output path %s is not a regular file; refusing", path)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return nil, fmt.Errorf("worker output path %s owned by uid %d; refusing", path, st.Uid)
		}
		return os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// maxArtifactRead bounds single artifact reads; excess is an explicit
// error, never silent truncation.
const maxArtifactRead = 64 << 20

// readConfinedFile reads abs only after it confines to root, then
// re-verifies the opened fd itself. The final O_NOFOLLOW open plus
// /proc/self/fd recheck closes the confine-then-read swap for the
// final component: a symlink planted (or swapped in) at abs is
// refused, not followed.
func readConfinedFile(root, abs string) ([]byte, error) {
	if err := confineToRoot(root, abs); err != nil {
		return nil, fmt.Errorf("artifact refused: %w", err)
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("artifact unreadable: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact refused: not a regular file")
	}
	fdPath := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	if real, err := filepath.EvalSymlinks(fdPath); err != nil {
		return nil, fmt.Errorf("artifact unreadable: %w", err)
	} else if err := confineToRoot(root, real); err != nil {
		return nil, fmt.Errorf("artifact refused after open: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxArtifactRead+1))
	if err != nil {
		return nil, fmt.Errorf("artifact unreadable: %w", err)
	}
	if len(data) > maxArtifactRead {
		return nil, fmt.Errorf("artifact exceeds %d-byte read cap", maxArtifactRead)
	}
	return data, nil
}
