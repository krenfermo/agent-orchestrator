package workflow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Verification context resolution — Checkpoint 8P-E.14.
//
// The incident this file exists for: a plan whose verification said
// `go build ./...` with workingDirectory "." ran at the repository root of a
// repo whose Go module actually lives in `backend/`. Go answered
//
//	pattern ./...: directory prefix . does not contain main module or its
//	selected dependencies
//
// with exit code 1, AO read "exit code 1" as "the code the worker produced is
// broken", and handed a perfectly good worktree to a fix worker. The worker
// (correctly) changed nothing, and the run ended in fix_no_verifiable_change →
// needs_attention.
//
// Two independent defects produced that: verify never resolved the command's
// working directory against the project's actual module root, and verify had no
// way to tell "your code is wrong" apart from "AO ran the check from the wrong
// place". Both are fixed here: resolution first (so the mistake usually never
// happens), classification second (so when it does happen, or when some other
// piece of AO's own verification infrastructure fails, it is never mistaken for
// a code defect).

const (
	// maxModuleSearchDepth bounds the module-root scan. Real multi-part repos
	// keep their module roots near the top (backend/, services/api/, go/src/);
	// scanning deeper mostly finds fixture modules under testdata.
	maxModuleSearchDepth = 4
	// maxVerifyContextRepairs bounds the durable self-heal loop per run: a
	// repair that does not fix the failure must never be attempted forever.
	maxVerifyContextRepairs = 2
	// maxTransientVerifyRetries bounds in-attempt retries of a transient
	// infrastructure failure (only for commands declared retry-safe).
	maxTransientVerifyRetries = 1
	// verifyContextRepairPhase is the durable_phase of the checkpoint written
	// whenever AO corrects a verification working directory by itself. It is
	// deliberately NOT an attention reason: nobody has a decision to make about
	// a repair that succeeded.
	verifyContextRepairPhase = "verify_context_repair"
)

// skippedModuleScanDirs are never descended into when looking for module roots.
// vendor/ and testdata/ in particular contain go.mod files that are not the
// project's module root, and treating one as the module root would be a worse
// failure than not resolving at all.
var skippedModuleScanDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "testdata": true,
	"dist": true, "build": true, "target": true, "third_party": true,
	".venv": true, "venv": true, ".ao": true, ".idea": true, ".cache": true,
}

var (
	errNoModuleRoot        = errors.New("no Go module root found in the worktree")
	errAmbiguousModuleRoot = errors.New("multiple Go module roots found in the worktree")
)

// VerifyContextResolution is the durable record of one command's
// working-directory decision: what the plan asked for, what actually ran, and
// why. Recorded for every command AO redirected — never for a command that ran
// exactly where the plan said.
type VerifyContextResolution struct {
	Label string `json:"label"`
	// Requested is the plan's workingDirectory ("." when unset).
	Requested string `json:"requested"`
	// Resolved is the worktree-relative directory the command actually ran in.
	Resolved string `json:"resolved"`
	// Reason explains the redirect in AO's own words.
	Reason string `json:"reason"`
	// Repaired distinguishes a pre-flight resolution (before any execution)
	// from a self-heal applied after a failed execution proved the directory
	// wrong.
	Repaired bool `json:"repaired,omitempty"`
	// RewrittenArgs records the package-path rebase, when one was needed.
	RewrittenArgs []string `json:"rewrittenArgs,omitempty"`
}

// VerifyInfraKind names what part of AO's verification infrastructure failed.
// Every value here means "the checks did not deliver a verdict about the code",
// which is why none of them may ever reach a fix worker.
type VerifyInfraKind string

const (
	// VerifyInfraWrongModuleRoot means the command ran outside the
	// module/project it was meant to verify. Self-heal candidate.
	VerifyInfraWrongModuleRoot VerifyInfraKind = "wrong_module_root"
	// VerifyInfraToolUnavailable means the verifier binary is missing, not
	// executable, or not on PATH.
	VerifyInfraToolUnavailable VerifyInfraKind = "tool_unavailable"
	// VerifyInfraConfigInvalid means the configured command/flags are not a
	// valid invocation of the tool at all.
	VerifyInfraConfigInvalid VerifyInfraKind = "config_invalid"
	// VerifyInfraRuntimeFailure means the process could not be run to
	// completion for an environmental reason (resource exhaustion, runtime
	// error).
	VerifyInfraRuntimeFailure VerifyInfraKind = "runtime_failure"
)

// VerifyInfraFailure is the structured "this was AO's fault, not the code's"
// record attached to a failed VerifyResult.
type VerifyInfraFailure struct {
	Kind      VerifyInfraKind `json:"kind"`
	Detail    string          `json:"detail"`
	Command   string          `json:"command"`
	Directory string          `json:"directory,omitempty"`
	// Transient means a bounded immediate retry is worth attempting.
	Transient bool `json:"transient,omitempty"`
	// Repairable means AO can deterministically correct its own verification
	// context and re-run (currently: wrong module root with a unique candidate).
	Repairable bool `json:"repairable,omitempty"`
}

// Reason renders the precise, human-readable stop reason for this failure.
func (f VerifyInfraFailure) Reason() string {
	dir := f.Directory
	if dir == "" {
		dir = "."
	}
	return fmt.Sprintf("verification infrastructure failure (%s) running %q in %q: %s", f.Kind, f.Command, dir, f.Detail)
}

// goModuleRootedCommand reports whether check is a Go invocation that must run
// from inside a module (or workspace) context. Anything else — npm, make, a
// project script — is left completely alone: this checkpoint deliberately fixes
// the one language whose failure mode it can recognize without fragile parsing.
func goModuleRootedCommand(check VerificationCommandCheck) bool {
	if strings.ToLower(filepath.Base(strings.TrimSpace(check.Command))) != "go" {
		return false
	}
	if len(check.Args) == 0 {
		return false
	}
	switch check.Args[0] {
	case "build", "vet", "test", "run", "list", "generate", "mod":
		return true
	}
	return false
}

// hasGoModuleContext reports whether dir (worktree-relative) already sits inside
// a Go module or workspace: dir itself, or any ancestor up to and including the
// worktree root, contains go.mod or go.work. `go build ./...` from a package
// subdirectory of a module is perfectly valid, so an explicitly configured
// working directory inside a module is left exactly as configured.
func hasGoModuleContext(root, rel string) bool {
	current := filepath.Join(root, filepath.FromSlash(rel))
	rootClean := filepath.Clean(root)
	for {
		if fileExists(filepath.Join(current, "go.mod")) || fileExists(filepath.Join(current, "go.work")) {
			return true
		}
		if filepath.Clean(current) == rootClean {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// discoverGoModuleRoot deterministically selects the Go module root under
// searchRoot. The algorithm is intentionally boring, because a wrong guess here
// would verify the wrong thing:
//
//  1. a go.work at searchRoot wins outright — it *is* the module context;
//  2. otherwise every go.mod under searchRoot is collected (bounded depth,
//     skipping vendor/testdata/node_modules and friends);
//  3. exactly one candidate → that is the module root;
//  4. several candidates that all live under one shallowest ancestor holding a
//     go.work → that ancestor;
//  5. anything else → ambiguous, and AO refuses to guess.
//
// excludeRel is the directory whose own go.mod must NOT be treated as a
// candidate. It is empty for a pre-flight resolution and set to the failing
// directory for a self-heal: a directory that has a go.mod and still made the
// toolchain say "does not contain main module" has just proved it is not the
// module root for this command, so the search continues below it.
//
// Returned paths are worktree-relative, slash-separated ("." for the root).
func discoverGoModuleRoot(root, searchRel, excludeRel string) (string, error) {
	base := filepath.Join(root, filepath.FromSlash(searchRel))
	if excludeRel != normalizeRel(searchRel) && fileExists(filepath.Join(base, "go.work")) {
		return normalizeRel(searchRel), nil
	}
	candidates, err := findGoModuleRoots(root, searchRel, excludeRel)
	if err != nil {
		return "", err
	}
	switch len(candidates) {
	case 0:
		return "", errNoModuleRoot
	case 1:
		return candidates[0], nil
	}
	if workRoot, ok := goWorkspaceCovering(root, candidates); ok {
		return workRoot, nil
	}
	return "", fmt.Errorf("%w: %s", errAmbiguousModuleRoot, strings.Join(candidates, ", "))
}

// findGoModuleRoots returns the worktree-relative directories under searchRel
// that contain a go.mod, shallowest first then alphabetically. A directory that
// holds go.mod is not descended into: nested modules under a module root are
// that module's business, not a second candidate.
func findGoModuleRoots(root, searchRel, excludeRel string) ([]string, error) {
	base := filepath.Join(root, filepath.FromSlash(searchRel))
	var found []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped, never fatal: it cannot contain a
			// module root AO is allowed to select anyway.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return fs.SkipDir
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(filepath.ToSlash(rel), "/"))
		}
		name := d.Name()
		if rel != "." && (skippedModuleScanDirs[name] || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		rootRel, rrErr := filepath.Rel(root, p)
		if rrErr != nil {
			return fs.SkipDir
		}
		dirRel := normalizeRel(filepath.ToSlash(rootRel))
		if fileExists(filepath.Join(p, "go.mod")) && dirRel != excludeRel {
			found = append(found, dirRel)
			return fs.SkipDir
		}
		if depth >= maxModuleSearchDepth {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool {
		di, dj := strings.Count(found[i], "/"), strings.Count(found[j], "/")
		if di != dj {
			return di < dj
		}
		return found[i] < found[j]
	})
	return found, nil
}

// goWorkspaceCovering returns the shallowest ancestor of every candidate that
// holds a go.work — the one case where several go.mod files still name a single
// unambiguous place to run from.
func goWorkspaceCovering(root string, candidates []string) (string, bool) {
	ancestor := candidates[0]
	for _, c := range candidates[1:] {
		ancestor = commonDirPrefix(ancestor, c)
	}
	for dir := ancestor; ; dir = parentRel(dir) {
		if fileExists(filepath.Join(root, filepath.FromSlash(dir), "go.work")) {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
	}
}

func commonDirPrefix(a, b string) string {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	i := 0
	for i < len(as) && i < len(bs) && as[i] == bs[i] {
		i++
	}
	if i == 0 {
		return "."
	}
	return strings.Join(as[:i], "/")
}

func parentRel(rel string) string {
	if rel == "." || !strings.Contains(rel, "/") {
		return "."
	}
	return path.Dir(rel)
}

func normalizeRel(rel string) string {
	rel = strings.TrimSpace(filepath.ToSlash(rel))
	if rel == "" {
		return "."
	}
	return path.Clean(rel)
}

// resolveVerifyCommandContext decides where one verification command runs.
//
// It only ever moves a command that AO can prove is in the wrong place: a Go
// module-rooted command whose configured directory is not inside any module or
// workspace. Everything else — a non-Go command, a Go command already inside a
// module, an explicitly configured directory that is valid — is returned
// unchanged with a nil resolution. When the directory does have to move,
// worktree-root-relative package arguments are rebased onto the new root so a
// scope-narrowed `go test ./backend/internal/foo/...` still names the same
// packages from inside `backend/`.
func resolveVerifyCommandContext(root string, check VerificationCommandCheck, forceSearch bool) (VerificationCommandCheck, *VerifyContextResolution, error) {
	requested := normalizeRel(check.WorkingDirectory)
	if !goModuleRootedCommand(check) {
		return check, nil, nil
	}
	if !forceSearch && hasGoModuleContext(root, requested) {
		return check, nil, nil
	}
	exclude := ""
	if forceSearch {
		exclude = requested
	}
	resolved, err := discoverGoModuleRoot(root, ".", exclude)
	if err != nil {
		return check, nil, err
	}
	if resolved == requested {
		return check, nil, nil
	}
	moved := check
	moved.WorkingDirectory = relForWorkingDirectory(resolved)
	rebased, changed := rebaseGoPackageArgs(check.Args, resolved)
	if changed {
		moved.Args = rebased
	}
	resolution := &VerifyContextResolution{
		Label:     commandLabel(check),
		Requested: requested,
		Resolved:  resolved,
		Reason:    fmt.Sprintf("%q is not inside a Go module; the module root is %q (go.mod)", requested, resolved),
	}
	if changed {
		resolution.RewrittenArgs = rebased
	}
	return moved, resolution, nil
}

// VerifyPathContext is the namespace one verification spec's relative paths
// live in — Checkpoint 8P-E.14B.
//
// The incident it exists for (wf-6528a538): a plan whose commands said
// `go test ./internal/postrunqa/...` with workingDirectory "." and whose file
// checks said `internal/postrunqa/classify.go`. Both halves were authored in
// the same namespace — the Go module, which lives in `backend/`. AO resolved
// the COMMAND into that module (". -> backend", args rebased) and both commands
// passed, then evaluated the FILE check against the worktree root, stat'd
// `<worktree>/internal/postrunqa`, and failed the whole verification with
// verify_environment_error over a file that was right where the plan said it
// was. The durable VerifyResult recorded both halves of the contradiction.
//
// The cause was an ownership boundary, not a bad stat: resolveVerifyCommandContext
// owned the working directory and the package arguments of ONE command, and
// nothing owned the spec. So this type owns the spec's namespace, both halves
// of it consult it, and they cannot disagree by construction.
type VerifyPathContext struct {
	// Base is the worktree-relative directory this spec's relative paths are
	// interpreted against. "." means the worktree root.
	Base string
}

// ResolvePath maps one declared path onto its worktree-relative location.
//
// The rule is deterministic and purely syntactic — it never consults the
// filesystem, so a path resolves to the same place whether or not the file
// happens to exist. That is what keeps a genuinely missing artifact a genuine
// verification failure instead of a namespace question:
//
//	Base == "."                      -> the path is already worktree-relative.
//	path is Base, or under Base/     -> already worktree-root-qualified; used
//	                                    as-is, so backend/internal/x.go can
//	                                    never become backend/backend/internal/x.go.
//	otherwise                        -> interpreted in Base, the namespace the
//	                                    spec's own commands run in.
//
// It is idempotent: ResolvePath(ResolvePath(p)) == ResolvePath(p), because the
// result always satisfies the second branch.
//
// The one ambiguity the rule cannot remove is a repository that genuinely has
// `backend/backend/`: there, a context-relative `backend/foo.go` is
// indistinguishable from a root-qualified one, and root-qualified wins. That is
// a documented, deterministic tie-break rather than a guess, and it is the same
// direction the command-argument rebase already takes.
func (vc VerifyPathContext) ResolvePath(rel string) string {
	p := normalizeRel(rel)
	base := normalizeRel(vc.Base)
	if base == "." || p == "." {
		return p
	}
	if p == base || strings.HasPrefix(p, base+"/") {
		return p
	}
	return path.Join(base, p)
}

// verifyPathContextFor derives a spec's namespace from the EFFECTIVE working
// directories of the commands that actually ran — after any pre-flight
// resolution and any mid-attempt self-heal, which is exactly why it is computed
// from the executed commands rather than from the plan text or from the
// recorded resolutions.
//
// The rule refuses to guess, in both directions:
//
//   - exactly one distinct effective directory -> that directory is the spec's
//     namespace, whether the plan asked for it (an explicit, valid
//     services/api) or AO resolved it (". -> backend");
//   - no commands at all, or commands that ran in different directories -> no
//     authoritative context, so paths stay worktree-relative. A spec whose
//     commands disagree has no single namespace to speak of, and inventing one
//     would be precisely the kind of guess this file exists to avoid.
func verifyPathContextFor(effectiveDirs []string) VerifyPathContext {
	base := ""
	for _, dir := range effectiveDirs {
		d := normalizeRel(dir)
		if base == "" {
			base = d
			continue
		}
		if base != d {
			return VerifyPathContext{Base: "."}
		}
	}
	if base == "" {
		return VerifyPathContext{Base: "."}
	}
	return VerifyPathContext{Base: base}
}

// relForWorkingDirectory renders a resolved directory for
// VerificationCommandCheck.WorkingDirectory, whose empty value means "the
// worktree root".
func relForWorkingDirectory(rel string) string {
	if rel == "." {
		return ""
	}
	return rel
}

// rebaseGoPackageArgs strips the module-root prefix from worktree-relative Go
// package patterns after the working directory moved into that module root:
// `./backend/internal/foo/...` run from `backend/` becomes
// `./internal/foo/...`. `./...` and everything that does not name the module
// root are left untouched.
func rebaseGoPackageArgs(args []string, moduleRel string) ([]string, bool) {
	if moduleRel == "." {
		return args, false
	}
	prefix := "./" + moduleRel + "/"
	out := make([]string, len(args))
	changed := false
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, prefix):
			out[i] = "./" + strings.TrimPrefix(a, prefix)
			changed = true
		case a == "./"+moduleRel || a == "./"+moduleRel+"/...":
			out[i] = "./..."
			changed = true
		default:
			out[i] = a
		}
	}
	if !changed {
		return args, false
	}
	return out, true
}

func commandLabel(check VerificationCommandCheck) string {
	return strings.Join(append([]string{check.Command}, check.Args...), " ")
}

// wrongModuleRootSignatures are the exact things the Go toolchain says when it
// was started outside the module it was asked about. They are matched on the
// command's own output — AO never infers "wrong directory" from an exit code.
var wrongModuleRootSignatures = []string{
	"does not contain main module",
	"go.mod file not found in current directory or any parent directory",
	"cannot find main module",
	"go work file not found",
	"outside available modules",
}

// execToolUnavailableSignatures mean the *runner* could not start the verifier
// binary at all. They are matched against the runner's error, where "no such
// file or directory" can only be about the binary or the directory.
var execToolUnavailableSignatures = []string{
	"executable file not found",
	"command not found",
	"no such file or directory",
	"permission denied",
	"exec format error",
	"cannot execute binary file",
	"is not recognized as an internal or external command",
}

// outputToolUnavailableSignatures are the deliberately narrower subset matched
// against a command's own stdout/stderr. "no such file or directory" and
// "permission denied" are excluded here on purpose: a genuinely failing test
// prints those about its own fixtures, and misreading that as an AO
// infrastructure failure would be the mirror image of the bug this file fixes.
var outputToolUnavailableSignatures = []string{
	"executable file not found",
	"command not found",
	"exec format error",
	"cannot execute binary file",
	"is not recognized as an internal or external command",
}

// configInvalidSignatures mean AO's configured invocation is not a valid use of
// the tool. No diff a fix worker could write would repair it.
var configInvalidSignatures = []string{
	"flag provided but not defined",
	"unknown command",
	"unknown subcommand",
	"go: unknown",
	"no such tool",
	"unknown flag",
	"unrecognized option",
}

// execTransientSignatures mean the process could not be started right now, for
// a reason worth trying again immediately.
var execTransientSignatures = []string{
	"resource temporarily unavailable",
	"text file busy",
	"too many open files",
	"cannot allocate memory",
	"interrupted system call",
	"connection reset by peer",
}

// outputTransientSignatures are the subset safe to recognize in a command's own
// output: host resource exhaustion, never anything a test could legitimately
// print about the system under test.
var outputTransientSignatures = []string{
	"resource temporarily unavailable",
	"text file busy",
	"too many open files",
	"cannot allocate memory",
}

// classifyVerifyExecutionFailure separates the two things a failed verification
// can mean. It returns nil when the failure is a genuine, code-caused failure —
// the only case a fix worker may ever be asked to repair — and a
// *VerifyInfraFailure when AO's own verification infrastructure is what broke.
//
// The classification reads only what actually happened: the runner's error, and
// the command's own output. It never guesses from the exit code alone, because
// exit code 1 is exactly what both a failing test and a wrong module root
// produce — the ambiguity that caused this incident.
func classifyVerifyExecutionFailure(check VerificationCommandCheck, dir string, exec VerifyCommandExecution, runErr error) *VerifyInfraFailure {
	label := commandLabel(check)
	if runErr != nil {
		out := strings.ToLower(runErr.Error())
		f := &VerifyInfraFailure{Command: label, Directory: dir, Detail: runErr.Error()}
		switch {
		case errors.Is(runErr, os.ErrNotExist) || containsAnySignature(out, execToolUnavailableSignatures):
			f.Kind = VerifyInfraToolUnavailable
		case containsAnySignature(out, execTransientSignatures):
			f.Kind, f.Transient = VerifyInfraRuntimeFailure, true
		default:
			f.Kind = VerifyInfraRuntimeFailure
		}
		return f
	}
	if exec.TimedOut {
		// A timeout is a real signal about the code under test (a hung test, a
		// runaway build) and keeps its existing repairable classification.
		return nil
	}
	out := strings.ToLower(exec.StderrTail + "\n" + exec.StdoutTail)
	switch {
	case containsAnySignature(out, wrongModuleRootSignatures):
		return &VerifyInfraFailure{
			Kind: VerifyInfraWrongModuleRoot, Command: label, Directory: dir,
			Detail: firstMatchingLine(exec, wrongModuleRootSignatures), Repairable: true,
		}
	case containsAnySignature(out, outputToolUnavailableSignatures):
		return &VerifyInfraFailure{
			Kind: VerifyInfraToolUnavailable, Command: label, Directory: dir,
			Detail: firstMatchingLine(exec, outputToolUnavailableSignatures),
		}
	case containsAnySignature(out, configInvalidSignatures):
		return &VerifyInfraFailure{
			Kind: VerifyInfraConfigInvalid, Command: label, Directory: dir,
			Detail: firstMatchingLine(exec, configInvalidSignatures),
		}
	case containsAnySignature(out, outputTransientSignatures):
		return &VerifyInfraFailure{
			Kind: VerifyInfraRuntimeFailure, Command: label, Directory: dir,
			Detail: firstMatchingLine(exec, outputTransientSignatures), Transient: true,
		}
	}
	return nil
}

func containsAnySignature(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// firstMatchingLine quotes the tool's own words for the durable record, so a
// person reading the stop reason sees the real message rather than AO's label
// for it.
func firstMatchingLine(exec VerifyCommandExecution, needles []string) string {
	for _, blob := range []string{exec.StderrTail, exec.StdoutTail} {
		for _, line := range strings.Split(blob, "\n") {
			if containsAnySignature(strings.ToLower(line), needles) {
				return strings.TrimSpace(line)
			}
		}
	}
	return strings.TrimSpace(exec.StderrTail)
}

// infraAttentionReason maps an infrastructure failure onto the canonical
// attention vocabulary, so a stopped run tells the user the one true thing they
// can act on: install the tool, or fix the verification configuration.
func infraAttentionReason(kind VerifyInfraKind) string {
	switch kind {
	case VerifyInfraToolUnavailable:
		return ReasonVerifyToolUnavailable
	case VerifyInfraRuntimeFailure:
		return ReasonVerifyInfraFailed
	default:
		return ReasonVerifyConfigInvalid
	}
}
