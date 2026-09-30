package practical3d

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// oracleSandboxProfile confines the oracle's execution of agent-authored code
// (Q1's build/test and Q4's hidden tests both compile and run the worker's
// commit). Agent code there must not reach the network (an unaccounted
// provider call), the experiment's private state (ledger, registry, other
// positions, the operator's oracle directory), provider credentials, or the
// staged oracle itself: during Q1 the staged oracle is unreadable; during Q4
// it is read-only.
const oracleSandboxProfile = `(version 1)
(allow default)

(deny network-outbound)
(deny network-bind)
(deny network-inbound)

(deny file-read* file-write* (subpath (param "AO_HOME")))
(deny file-read* file-write* (subpath (param "AO_SRC")))
(deny file-read* file-write* (subpath (param "ORACLE_DIR")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/.claude")))
(deny file-read* file-write* (prefix (string-append (param "REAL_HOME") "/.claude.json")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/.codex")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/Library/Keychains")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/Library/Application Support/Claude")))
(deny process-exec (literal "/usr/bin/security") (literal "/bin/launchctl") (literal "/usr/bin/osascript") (literal "/usr/bin/open"))

; The staged oracle: hidden during Q1, read-only during Q4 (its scratch
; directory stays writable).
(deny file-read* file-write* (subpath (param "STAGE_HIDDEN")))
(deny file-write* (subpath (param "STAGE_RO")))
(allow file-write* (subpath (param "STAGE_SCRATCH")))
`

// oracleStage is a private copy of the frozen oracle whose bytes are verified
// against the manifest before every use.
type oracleStage struct {
	root, script, hidden, scratch, home string
}

// stageOracle copies oracle.sh and the task's hidden tests into a new private
// directory and verifies the copied bytes (not the operator's files, which
// could change between the check and the use).
func stageOracle(o RealOracle, task, wantHidden string) (*oracleStage, error) {
	root, err := os.MkdirTemp("/private/tmp", "ao3dp-oracle-")
	if err != nil {
		return nil, err
	}
	s := &oracleStage{root: root, script: filepath.Join(root, "oracle", "oracle.sh"), hidden: filepath.Join(root, "oracle", "hidden"), scratch: filepath.Join(root, "oracle", "tmp"), home: filepath.Join(root, "home")}
	for _, d := range []string{filepath.Join(s.hidden, task), s.scratch, s.home, filepath.Join(root, "gocache"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	raw, err := os.ReadFile(o.Script)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.script, raw, 0o500); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(o.HiddenDir, task))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(o.HiddenDir, task, e.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(s.hidden, task, e.Name()), b, 0o400); err != nil {
			return nil, err
		}
	}
	return s, s.verify(o.Manifest, task, wantHidden)
}

// verify checks the staged bytes against the frozen Q4 oracle.
func (s *oracleStage) verify(m Manifest, task, wantHidden string) error {
	script, err := os.ReadFile(s.script)
	if err != nil {
		return err
	}
	if sha256Hex(script) != m.Q4Oracle.CommandSHA256 || sha256Hex(script) != m.Q4Oracle.RunnerImageOrBinarySHA256 {
		return errors.New("staged oracle script differs from the frozen Q4 oracle")
	}
	hidden, err := BuildHiddenManifest(s.hidden, task)
	if err != nil {
		return err
	}
	if sha256Hex(hidden) != wantHidden {
		return errors.New("staged hidden tests differ from the frozen manifest")
	}
	return nil
}

func (s *oracleStage) cleanup() { _ = os.RemoveAll(s.root) }

// command builds a sandboxed oracle command. q4 selects the Q4 view of the
// staged oracle (read-only) instead of Q1's (hidden).
func (s *oracleStage) command(ctx context.Context, o RealOracle, q4 bool, dir string, argv ...string) (*exec.Cmd, error) {
	profile := filepath.Join(s.root, "oracle.sb")
	if _, err := os.Stat(profile); err != nil {
		if err := os.WriteFile(profile, []byte(oracleSandboxProfile), 0o400); err != nil {
			return nil, err
		}
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	staged := filepath.Join(s.root, "oracle")
	hide, ro := staged, filepath.Join(s.root, ".none")
	if q4 {
		hide, ro = filepath.Join(s.root, ".none"), staged
	}
	params := map[string]string{"AO_HOME": filepath.Join(realHome, ".ao"), "AO_SRC": o.Exec.Cfg.AOSrc, "ORACLE_DIR": o.Exec.Cfg.OracleDir, "REAL_HOME": realHome,
		"STAGE_HIDDEN": hide, "STAGE_RO": ro, "STAGE_SCRATCH": s.scratch}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" || !filepath.IsAbs(v) {
			return nil, fmt.Errorf("oracle sandbox parameter %s is not an absolute path", k)
		}
		params[k] = resolveOrSelf(v)
		keys = append(keys, k)
	}
	sort.Strings(keys)
	full := []string{"-f", profile}
	for _, k := range keys {
		full = append(full, "-D", k+"="+params[k])
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append(full, argv...)...)
	cmd.Dir = dir
	// A scrubbed environment: nothing of the supervisor's (credentials,
	// paths, AO variables) reaches agent code.
	cmd.Env = []string{"PATH=/usr/local/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin", "HOME=" + s.home, "TMPDIR=" + filepath.Join(s.root, "tmp") + "/",
		"GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=", "GOPROXY=off", "GOCACHE=" + filepath.Join(s.root, "gocache"), "GOPATH=" + filepath.Join(s.home, "go"), "LANG=en_US.UTF-8"}
	return cmd, nil
}
