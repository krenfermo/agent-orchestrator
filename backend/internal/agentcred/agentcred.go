// Package agentcred is the handoff between the daemon that MINTS an agent
// credential and the `ao` CLI that PRESENTS it from inside the agent's pane.
//
// It is a leaf package on purpose. Both ends need the same three facts -- the
// env var that names the credential file, the file's shape, and the marker that
// says "this process is running inside an AO-launched agent" -- and neither the
// CLI nor the daemon should have to import the other to agree on them.
//
// The credential rests in a file rather than in an environment variable because
// an env var is readable from the process table by anything running as this
// user, is inherited by every tool the agent shells out to, and shows up in
// crash dumps and `ps eww` output. A 0600 file under the daemon's data dir is
// the same trust boundary the CLI's own credential already uses.
package agentcred

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// EnvCredentialFile names the file holding this agent's credential. The
	// value is the NAME of an environment variable, not a credential; the
	// secret lives in the 0600 file this points at and never in the process
	// environment, which is the whole reason this indirection exists.
	EnvCredentialFile = "AO_AGENT_CREDENTIAL_FILE" //nolint:gosec // G101: env var name, not a credential.
	// EnvSessionOwner is the ownership token the tmux runtime stamps onto every
	// session AO creates (adapters/runtime/tmux). Its PRESENCE is the reliable
	// answer to "am I running inside an AO-launched agent?", which is the
	// question that decides whether the CLI may fall back on a person's
	// credential -- see ports.go's InsideAgentRuntime.
	EnvSessionOwner = "AO_SESSION_OWNER"
)

// File is what the daemon writes and the CLI reads. Everything but Token is
// there so a person looking at the file can tell what it is and what it may do;
// nothing here is read back by the daemon, which trusts only the token's hash.
type File struct {
	Token          string    `json:"token"`
	CredentialID   string    `json:"credentialId"`
	Role           string    `json:"role"`
	ProjectID      string    `json:"projectId"`
	SessionID      string    `json:"sessionId"`
	WorkflowRunID  string    `json:"workflowRunId,omitempty"`
	WorkflowStepID string    `json:"workflowStepId,omitempty"`
	ReviewRunID    string    `json:"reviewRunId,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

// Dir is where credential files live under a data dir.
func Dir(dataDir string) string { return filepath.Join(dataDir, "agent-credentials") }

// Path is the file for one runtime handle. The handle, not the review run id,
// so a pane can find its own credential from the env var alone and a stale file
// from a previous launch of the same name is overwritten rather than accumulated.
func Path(dataDir, handleID string) string {
	return filepath.Join(Dir(dataDir), sanitize(handleID)+".json")
}

// Write persists a credential file at path with 0600, creating its directory
// with 0700. It writes through a temporary file in the same directory so a
// reader never observes a half-written credential.
func Write(path string, f File) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("agent credential: a path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("agent credential: create directory: %w", err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("agent credential: encode: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agentcred-*")
	if err != nil {
		return fmt.Errorf("agent credential: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent credential: restrict temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent credential: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent credential: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("agent credential: install: %w", err)
	}
	return nil
}

// Read loads a credential file. A missing file is (zero, false, nil): an agent
// launched by a build that could not mint one is a real, expected state, not an
// error.
func Read(path string) (File, bool, error) {
	if strings.TrimSpace(path) == "" {
		return File{}, false, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path comes from AO's own env handoff.
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, false, nil
		}
		return File{}, false, fmt.Errorf("agent credential: read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return File{}, false, fmt.Errorf("agent credential: decode %s: %w", path, err)
	}
	if strings.TrimSpace(f.Token) == "" {
		return File{}, false, nil
	}
	return f, true, nil
}

// Remove deletes a credential file, ignoring a file that is already gone.
func Remove(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agent credential: remove %s: %w", path, err)
	}
	return nil
}

// InsideAgentRuntime reports whether this process is running inside a runtime AO
// launched for an agent.
//
// It is what stops the CLI reaching for a PERSON's credential from inside a
// pane. An agent that cannot authenticate as itself must fail and say so, not
// quietly borrow the operator's session and act with the operator's authority
// over every project in the installation.
func InsideAgentRuntime(lookup func(string) (string, bool)) bool {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if v, ok := lookup(EnvCredentialFile); ok && strings.TrimSpace(v) != "" {
		return true
	}
	v, ok := lookup(EnvSessionOwner)
	return ok && strings.TrimSpace(v) != ""
}

// sanitize keeps a handle usable as a file name without inventing a new
// identity for it: only characters that are already safe survive, and anything
// else becomes a dash.
func sanitize(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agent"
	}
	return out
}
