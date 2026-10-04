// Package gitignoreprobe answers, for one repository, which of a set of paths
// git ignores — and which rule does the ignoring.
//
// It exists for workflow's pre-dispatch deliverable-observability check
// (internal/workflow/deliverable_observability.go), and it asks git rather than
// re-implementing `.gitignore`. Nothing else would be correct: the rules are
// layered (repository files at every directory level, `.git/info/exclude`, the
// user's `core.excludesFile`), order-dependent within a file, and carry
// negation — and AO's answer has to be git's answer, because git is what will
// later fail to show the work.
//
// Read-only by construction: it runs exactly one plumbing command, never
// writes, and never touches an index or a working tree.
package gitignoreprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Probe implements workflow.DeliverableIgnoreProbe over the git CLI.
//
// The zero value is usable and runs `git` from PATH.
type Probe struct {
	// GitBinary overrides the executable, for tests. Empty means "git".
	GitBinary string
}

// IgnoredPaths reports which of paths the repository at repoPath ignores.
//
// It shells out once:
//
//	git -C <repo> check-ignore -v -z --stdin
//
// with the paths NUL-separated on stdin, and reads back NUL-separated
// (source, line, pattern, pathname) records.
//
// Three decisions here are the whole correctness of the check:
//
//   - `--no-index` is NOT passed. Without it git omits TRACKED paths entirely,
//     which is the answer AO needs: a tracked file's changes are visible to git
//     no matter what pattern would otherwise have matched it. Passing
//     `--no-index` would report such a path as ignored and refuse a dispatch
//     over work git can see perfectly well.
//   - a record whose pattern begins with `!` is a NEGATION: the rule matched,
//     and its effect is to re-include the path. `-v` reports it like any other
//     match, so it is dropped here. Reading it as "ignored" would invert every
//     exception in every .gitignore in the repository.
//   - exit status 1 means "none of them are ignored" and is not an error. Only
//     status 2 and above are.
func (p *Probe) IgnoredPaths(ctx context.Context, repoPath string, paths []string) ([]workflow.IgnoredDeliverable, error) {
	if strings.TrimSpace(repoPath) == "" {
		return nil, fmt.Errorf("gitignoreprobe: repository path is required")
	}
	cleaned := make([]string, 0, len(paths))
	for _, raw := range paths {
		if s := strings.TrimSpace(raw); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return nil, nil
	}

	bin := p.GitBinary
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, "-C", repoPath, "check-ignore", "-v", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(cleaned, "\x00") + "\x00")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("gitignoreprobe: git check-ignore in %q: %w: %s",
				repoPath, err, strings.TrimSpace(stderr.String()))
		}
		// Exit 1 with no output: nothing is ignored.
		return nil, nil
	}
	return parseCheckIgnore(stdout.Bytes())
}

// parseCheckIgnore reads `check-ignore -v -z` output: four NUL-terminated
// fields per record, in the order source, line, pattern, pathname.
//
// A record it cannot read is dropped rather than guessed at. This function only
// ever REMOVES refusals, so a malformed record costs the old behaviour (the
// dispatch proceeds) and never invents one.
func parseCheckIgnore(out []byte) ([]workflow.IgnoredDeliverable, error) {
	if len(out) == 0 {
		return nil, nil
	}
	fields := strings.Split(string(out), "\x00")
	// A trailing NUL leaves an empty final element.
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	if len(fields)%4 != 0 {
		return nil, fmt.Errorf("gitignoreprobe: git check-ignore returned %d fields, want a multiple of 4", len(fields))
	}
	var ignored []workflow.IgnoredDeliverable
	for i := 0; i < len(fields); i += 4 {
		source, rawLine, pattern, pathname := fields[i], fields[i+1], fields[i+2], fields[i+3]
		if pathname == "" {
			continue
		}
		// The negation case. See the doc comment on IgnoredPaths.
		if strings.HasPrefix(pattern, "!") {
			continue
		}
		// An empty pattern with an empty source is git reporting that it found
		// no rule at all for this path, which it does under --non-matching. AO
		// does not pass that flag, but a record shaped like one is still not an
		// ignore and must not be read as one.
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		line, err := strconv.Atoi(rawLine)
		if err != nil {
			line = 0
		}
		ignored = append(ignored, workflow.IgnoredDeliverable{
			Path:       pathname,
			RuleSource: source,
			RuleLine:   line,
			Pattern:    pattern,
		})
	}
	return ignored, nil
}
