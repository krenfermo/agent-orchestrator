// Package runtimehome prepares an AO-owned, per-user filesystem root that
// every provider CLI subprocess launched on that user's behalf runs
// against, so credentials for one AO user are never visible to another
// (Checkpoint 8P-B). It never touches the daemon process's own real
// $HOME/OS-default app-data locations -- see CLAUDE.md's "App state lives
// under ~/.ao only" hard rule.
package runtimehome

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var safeUserID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Environment is one user's isolated runtime filesystem, rooted at
// AO_DATA_DIR/users/<user-id>/.
type Environment struct {
	UserID domain.UserID
	// Root is AO_DATA_DIR/users/<user-id>.
	Root string
	// RuntimeHome is used as HOME (and the XDG_*/TMPDIR fallback base) for
	// any subprocess launched on this user's behalf, so a provider CLI with
	// no explicit config-dir override still can't see another user's real
	// home directory.
	RuntimeHome string
	// ProvidersDir holds one subdirectory per provider harness
	// (providers/claude-code, providers/codex, ...), used as that
	// provider's own native config-dir override when it supports one.
	ProvidersDir string
	// SessionsDir holds per-session scratch state scoped to this user.
	SessionsDir string
	// ClaudeConfigDir is handed to Claude Code as CLAUDE_CONFIG_DIR.
	ClaudeConfigDir string
	// CodexHome is handed to Codex as CODEX_HOME.
	CodexHome string
	// TempRoot is this user's isolated TMPDIR.
	TempRoot string
}

// Prepare creates (idempotently) the directory layout described in
// Checkpoint 8P-B for userID under dataDir (AO_DATA_DIR) and returns it.
// Every directory is created private (0o700) since it may come to hold CLI
// credential material.
func Prepare(dataDir string, userID domain.UserID) (Environment, error) {
	if dataDir == "" || !filepath.IsAbs(dataDir) {
		return Environment{}, errors.New("runtimehome: absolute AO data directory is required")
	}
	if !safeUserID.MatchString(string(userID)) {
		return Environment{}, errors.New("runtimehome: invalid user id")
	}
	root := filepath.Join(dataDir, "users", string(userID))
	env := Environment{
		UserID:          userID,
		Root:            root,
		RuntimeHome:     filepath.Join(root, "runtime-home"),
		ProvidersDir:    filepath.Join(root, "providers"),
		SessionsDir:     filepath.Join(root, "sessions"),
		ClaudeConfigDir: filepath.Join(root, "providers", "claude-code"),
		CodexHome:       filepath.Join(root, "providers", "codex"),
		TempRoot:        filepath.Join(root, "runtime-home", "tmp"),
	}
	dirs := []string{
		env.Root, env.RuntimeHome, env.ProvidersDir, env.SessionsDir,
		env.ClaudeConfigDir, env.CodexHome, env.TempRoot,
		filepath.Join(env.RuntimeHome, "config"),
		filepath.Join(env.RuntimeHome, "state"),
		filepath.Join(env.RuntimeHome, "cache"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Environment{}, err
		}
	}
	ensureIsolatedKeychain(env)
	return env, nil
}

// SubprocessEnv returns the env var overrides a provider subprocess started
// on this user's behalf must receive so it never resolves into another
// user's (or the daemon host's) real credential/config locations. Callers
// merge this into the launch env (letting these keys win); it deliberately
// never mutates process-global env (os.Setenv) -- see CLAUDE.md/AGENTS.md.
func (e Environment) SubprocessEnv() map[string]string {
	return map[string]string{
		"HOME":              e.RuntimeHome,
		"XDG_CONFIG_HOME":   filepath.Join(e.RuntimeHome, "config"),
		"XDG_STATE_HOME":    filepath.Join(e.RuntimeHome, "state"),
		"XDG_CACHE_HOME":    filepath.Join(e.RuntimeHome, "cache"),
		"TMPDIR":            e.TempRoot,
		"TEMP":              e.TempRoot,
		"TMP":               e.TempRoot,
		"CLAUDE_CONFIG_DIR": e.ClaudeConfigDir,
		"CODEX_HOME":        e.CodexHome,
	}
}
