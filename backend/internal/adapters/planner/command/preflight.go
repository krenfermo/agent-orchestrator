package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// preflight.go — the planner's launch contract, part one.
//
// THE INCIDENT (wf-7f8cc736). A real objective stopped before planning with
// "The planner could not be started. Check the planner provider's auth and
// installation, then retry planning." The durable evidence said the planner
// subprocess had run for 33s, spent zero tokens, made zero API calls and
// exited 1 — and nothing else, because the only text the adapter attached to
// the failure was the first 500 bytes of the CLI's JSON envelope, which is
// `duration_api_ms`, `session_id` and a usage block. The fields that say what
// went wrong (`is_error`, `subtype`, `api_error_status`, `result`) live at
// ~1500 bytes in that envelope and were cut off by construction, on every
// planner failure AO has ever recorded.
//
// So the adapter could not tell a missing binary from an expired credential
// from a provider overload, and neither could the coordinator: its
// provider-failure classifier reads text, and the text it was handed was
// metrics. Every planner failure collapsed onto one nonrecoverable stop.
//
// This file is the half of the fix that runs BEFORE the subprocess: it
// resolves the executable and the profile directory the launch would use, from
// the exact environment the launch would inherit, and returns a typed failure
// naming which one is wrong. That is what lets AO say "Claude Code is not
// installed at the path the daemon can see" instead of "check auth and
// installation".
//
// It deliberately does NOT probe credentials. A preflight that shells out to
// the provider to ask whether it is logged in would double every planner call's
// cost and latency, and would still be a guess by the time the real call runs.
// Auth is diagnosed from the provider's OWN verdict on the real invocation —
// see diagnosis.go.

// launchPlan is the resolved, inspectable shape of exactly one planner
// invocation: the single canonical launch contract every planner start goes
// through. Nothing here is a secret — an executable path, a working directory
// and the NAMES of the profile variables that were resolved — so all of it is
// safe to put in a stop a person reads.
type launchPlan struct {
	// Binary is what the caller asked for (e.g. "claude"); BinaryPath is what
	// it resolved to on disk.
	Binary     string
	BinaryPath string
	Args       []string
	Dir        string
	Env        []string
	// ProfileVar is the environment variable that decided where the provider
	// will look for its configuration and credentials ("CLAUDE_CONFIG_DIR",
	// "CODEX_HOME", or "HOME"), and ProfileDir the directory it named. Empty
	// when the subprocess inherits no home at all, which is itself a fault.
	ProfileVar string
	ProfileDir string
}

// preflight resolves the launch contract for one invocation and refuses it
// with a typed error when the provider could not possibly work.
//
// The environment it inspects is the MERGED env the subprocess will actually
// receive — not the daemon's own. That distinction is the whole point: an
// isolated per-user runtime home overrides HOME and CLAUDE_CONFIG_DIR, and a
// preflight that read os.Getenv would happily bless a launch that is about to
// run against a directory that does not exist.
func preflight(ctx context.Context, binary string, args []string, dir string, env []string, fallback func(context.Context) (string, error)) (launchPlan, error) {
	plan := launchPlan{Binary: binary, Args: args, Dir: dir, Env: env}
	path, err := resolveExecutable(binary, env)
	if err != nil && fallback != nil {
		// PATH did not have it. Before declaring the provider missing, probe the
		// same well-known install locations every agent adapter probes -- the
		// asymmetry this closes is a daemon whose workers launch and whose
		// planner does not, purely because one path searched ~/.local/bin and
		// the other did not.
		if found, ferr := fallback(ctx); ferr == nil && found != "" {
			if xerr := executableFile(found); xerr == nil {
				plan.BinaryPath = found
				plan.ProfileVar, plan.ProfileDir = profileDir(binary, env)
				return plan, profileReadable(plan)
			}
		}
	}
	if err != nil {
		return plan, err
	}
	plan.BinaryPath = path
	plan.ProfileVar, plan.ProfileDir = profileDir(binary, env)
	return plan, profileReadable(plan)
}

// profileReadable is the second half of the preflight, shared by both binary
// resolution paths so a fallback-resolved provider gets exactly the same
// profile check as a PATH-resolved one.
func profileReadable(plan launchPlan) error {
	if plan.ProfileDir == "" {
		return nil
	}
	if err := readableDir(plan.ProfileDir); err != nil {
		return fmt.Errorf("%w: %s=%s: %w", ports.ErrPlannerRuntimeHomeUnreadable, plan.ProfileVar, plan.ProfileDir, err)
	}
	return nil
}

// resolveExecutable finds binary using the PATH the subprocess will inherit.
//
// os/exec.LookPath cannot be used here: it reads the DAEMON's PATH from the
// process environment, and the whole reason this function exists is that the
// subprocess may be handed a different one. A binary that contains a separator
// is taken as a path and checked directly, matching exec's own rule.
func resolveExecutable(binary string, env []string) (string, error) {
	if strings.TrimSpace(binary) == "" {
		return "", fmt.Errorf("%w: no planner binary configured", ports.ErrPlannerBinaryMissing)
	}
	if strings.ContainsRune(binary, os.PathSeparator) {
		if err := executableFile(binary); err != nil {
			return "", fmt.Errorf("%w: %s: %w", ports.ErrPlannerBinaryMissing, binary, err)
		}
		return binary, nil
	}
	pathVar := envValue(env, "PATH")
	if pathVar == "" {
		return "", fmt.Errorf("%w: %q, and the planner subprocess would inherit an empty PATH", ports.ErrPlannerBinaryMissing, binary)
	}
	entries := filepath.SplitList(pathVar)
	// "Found but unusable" is a different fact from "absent", and it is the
	// one a person can act on ("chmod +x", "that is a directory"), so a
	// candidate that exists under the right name is remembered and reported
	// instead of being silently skipped into a "not on PATH" verdict.
	var unusable error
	for _, entry := range entries {
		if entry == "" {
			entry = "."
		}
		candidate := filepath.Join(entry, binary)
		err := executableFile(candidate)
		if err == nil {
			return candidate, nil
		}
		if unusable == nil && !os.IsNotExist(err) {
			unusable = fmt.Errorf("%s: %w", candidate, err)
		}
	}
	if unusable != nil {
		return "", fmt.Errorf("%w: %w", ports.ErrPlannerBinaryMissing, unusable)
	}
	return "", fmt.Errorf("%w: %q is not on the PATH the planner subprocess would inherit (%d entries)",
		ports.ErrPlannerBinaryMissing, binary, len(entries))
}

// executableFile reports whether path is a regular file with an execute bit.
// A directory or a non-executable file named like the CLI is a "found but
// unusable" installation, which the caller must not report as "missing".
func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory, not an executable")
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("found but not executable (mode %s)", info.Mode().Perm())
	}
	return nil
}

// profileDir names the directory the provider CLI will read its configuration
// and credentials from, and the variable that decided it. The provider-specific
// overrides are checked first because that is the order the CLIs themselves
// use; HOME is the fallback every provider shares.
//
// It is keyed off the binary NAME rather than a harness enum on purpose: this
// package launches whatever binary AO_PLANNER_BIN names, and a Codex planner
// must get the same treatment as a Claude one without this file learning about
// workflow's harness vocabulary.
func profileDir(binary string, env []string) (string, string) {
	name := strings.ToLower(filepath.Base(binary))
	candidates := make([]string, 0, 2)
	switch {
	case strings.Contains(name, "codex"):
		candidates = append(candidates, "CODEX_HOME")
	case strings.Contains(name, "claude"):
		candidates = append(candidates, "CLAUDE_CONFIG_DIR")
	}
	candidates = append(candidates, "HOME")
	for _, key := range candidates {
		if v := envValue(env, key); v != "" {
			return key, v
		}
	}
	return "", ""
}

// readableDir checks that dir exists, is a directory, and can be listed.
// Existence alone is not enough: the TrustedLocal incident's failure mode was a
// runtime home that existed and held none of the credentials the provider
// needed, and the first observable symptom of that class is an unreadable or
// empty profile directory.
func readableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("is not a directory")
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// io.EOF means "readable and empty", which is a legitimate state for a
	// freshly prepared runtime home; anything else is a real read failure.
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// envValue reads one key out of a "KEY=VALUE" slice, last writer winning —
// the same precedence execve applies, and the same one mergeEnv relies on.
func envValue(env []string, key string) string {
	out := ""
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == key {
			out = v
		}
	}
	return out
}

// resolveLaunch is Planner's entry into the preflight above.
//
// It skips the on-disk checks when runCommand is injected, and that is a
// statement about what the checks MEAN rather than a concession to tests: the
// preflight verifies the provider installation this adapter is about to exec,
// and an injected runner is not going to exec anything. Verifying that some
// binary named "claude" happens to exist on the machine running the process
// would make the outcome depend on the host rather than on the code under it,
// which is how a suite starts passing for the wrong reason.
//
// The resolved argv, directory and environment are still filled in on both
// paths, so evidence and error text are identical either way.
func (p Planner) resolveLaunch(ctx context.Context, args []string, dir string, env []string) (launchPlan, error) {
	if p.runCommand != nil {
		return launchPlan{Binary: p.Binary, Args: args, Dir: dir, Env: env}, nil
	}
	return preflight(ctx, p.Binary, args, dir, env, p.ResolveFallback)
}

// executable is what the subprocess actually runs: the path the preflight
// resolved, falling back to the configured name when no preflight ran (an
// injected runner, which resolves nothing because it execs nothing).
func (p launchPlan) executable() string {
	if p.BinaryPath != "" {
		return p.BinaryPath
	}
	return p.Binary
}
