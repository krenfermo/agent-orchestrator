package githubintel

// git.go -- the half of "repository state" that needs no network.
//
// Branch, HEAD, dirtiness and ahead/behind are facts about the checkout on
// this machine, and reading them from GitHub would be both slower and wrong:
// GitHub knows what was pushed, not what the developer has in front of them.
// So the local facts come from git and the remote facts come from the API, and
// the two are reported side by side rather than merged into a single number
// that hides which is which.
//
// Every command here is read-only, runs with an explicit timeout, and returns
// zero values rather than errors for the ordinary "not a repo / no upstream"
// cases -- those are states to report, not failures to raise.

import (
	"context"
	"strconv"
	"strings"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// gitTimeout bounds one git invocation. These are all local metadata reads
// that finish in milliseconds; anything slower is a wedged repository we
// decline to block a dispatch on.
const gitTimeout = 5 * time.Second

// Commit is one commit from the local history.
type Commit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

// LocalState is everything git can say about a checkout without a network
// round trip.
type LocalState struct {
	// Available is false when the path is not a git repository AO can read.
	Available bool
	Branch    string
	HeadSHA   string
	// Detached reports a HEAD that is not on a branch.
	Detached bool
	// UpstreamRef is the remote-tracking branch, e.g. "origin/main". Empty
	// when the branch tracks nothing, which is normal on a fresh branch.
	UpstreamRef string
	// UpstreamSHA is the tip of that remote-tracking ref AS OF THE LAST FETCH.
	// It is emphatically not the server's current tip -- see Snapshot.Remote
	// for that -- and the two are reported separately because a stale
	// remote-tracking ref is itself a fact worth seeing.
	UpstreamSHA string
	// Ahead and Behind count commits relative to UpstreamRef.
	Ahead, Behind int
	// Dirty reports uncommitted changes in the working tree.
	Dirty         bool
	OriginURL     string
	RecentCommits []Commit
}

// Git reads local repository state. The interface exists so the service can be
// tested without a real checkout.
type Git interface {
	Read(ctx context.Context, dir string, commitLimit int) LocalState
}

// ExecGit is the production Git, shelling out to the git binary.
type ExecGit struct{}

// Read collects the local state of the repository at dir. It never returns an
// error: a directory that is not a repository, or a git that is not installed,
// is reported as Available=false, which is a state the surface renders.
func (ExecGit) Read(ctx context.Context, dir string, commitLimit int) LocalState {
	var out LocalState
	if strings.TrimSpace(dir) == "" {
		return out
	}
	if _, ok := git(ctx, dir, "rev-parse", "--git-dir"); !ok {
		return out
	}
	out.Available = true

	if branch, ok := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); ok {
		if branch == "HEAD" {
			out.Detached = true
		} else {
			out.Branch = branch
		}
	}
	out.HeadSHA, _ = git(ctx, dir, "rev-parse", "HEAD")
	out.OriginURL, _ = git(ctx, dir, "remote", "get-url", "origin")

	if ref, ok := git(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); ok {
		out.UpstreamRef = ref
		out.UpstreamSHA, _ = git(ctx, dir, "rev-parse", ref)
		// --left-right --count HEAD...upstream prints "<ahead>\t<behind>".
		if counts, ok := git(ctx, dir, "rev-list", "--left-right", "--count", "HEAD..."+ref); ok {
			fields := strings.Fields(counts)
			if len(fields) == 2 {
				out.Ahead, _ = strconv.Atoi(fields[0])
				out.Behind, _ = strconv.Atoi(fields[1])
			}
		}
	}
	if status, ok := git(ctx, dir, "status", "--porcelain"); ok {
		out.Dirty = strings.TrimSpace(status) != ""
	}
	out.RecentCommits = readCommits(ctx, dir, commitLimit)
	return out
}

// commitFormat is a unit-separated log format: a subject can contain anything,
// so the fields are split on a byte that cannot appear in one.
const commitFormat = "%H\x1f%s\x1f%an\x1f%aI"

func readCommits(ctx context.Context, dir string, limit int) []Commit {
	if limit <= 0 {
		return nil
	}
	out, ok := git(ctx, dir, "log", "-n", strconv.Itoa(limit), "--pretty=format:"+commitFormat)
	if !ok || out == "" {
		return nil
	}
	commits := make([]Commit, 0, limit)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 4 {
			continue
		}
		commit := Commit{SHA: fields[0], Subject: fields[1], Author: fields[2]}
		if t, err := time.Parse(time.RFC3339, fields[3]); err == nil {
			commit.Date = t.UTC()
		}
		commits = append(commits, commit)
	}
	return commits
}

// git runs one read-only git command and returns its trimmed stdout. ok=false
// means the command failed, which every caller treats as "this fact is not
// available" rather than as an error to propagate.
func git(ctx context.Context, dir string, args ...string) (string, bool) {
	cmdCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	out, err := aoprocess.CommandContext(cmdCtx, "git", full...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
