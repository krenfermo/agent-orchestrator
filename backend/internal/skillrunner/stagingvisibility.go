package skillrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stagingvisibility.go — proving the container can actually see what AO staged.
//
// THE INCIDENT. The first static-code pilot failed at the last possible moment
// with "the run did not demonstrate filesystem_isolation". The cause was three
// layers away from that sentence: the container runtime on that host was Colima,
// a VM that shares only the host paths named in its own config, and the staging
// root was outside them. The bind mount succeeded — Docker happily mounts a path
// the VM cannot see — and produced an EMPTY directory. AO then counted zero
// input files, could not attest filesystem isolation, and refused.
//
// Refusing was right. The message was not: it named a control, not the mount,
// and it arrived after an image had been resolved, a checkout copied, and a
// container started. Nothing about it pointed at the VM's share list.
//
// So this file adds two things, and they answer different questions:
//
//   - VerifyStagingVisible, BEFORE anything is staged: can the runtime see this
//     host directory at all? A sentinel file, written by AO and read back from
//     inside a container. It is a fact measured through the runtime, not a
//     configuration file AO trusts — a declared mount that silently does not
//     work is exactly the shape of this incident.
//
//   - stagedInputsDigest, AFTER the run: are the files the container saw THE
//     files AO staged? A count matches by accident; a digest over the sorted
//     (path, size, content-hash) triples does not. A mount that carried a
//     partial or stale tree is caught here rather than being scanned and
//     reported as if it were the project.
//
// Neither widens what a run may reach. The probe mounts the staging root and
// nothing else, read-only, on an approved image, with no network.

// ErrStagingNotVisible is the runtime saying it cannot see the host directory
// AO intends to stage into. It is distinct from ErrStagingUnusable — the path
// is fine, the SHARE is missing — because the remedies are different: one is a
// permissions or layout problem on the host, the other is a line in the
// runtime's config.
var ErrStagingNotVisible = fmt.Errorf("skillrunner: the container runtime cannot see the staging directory")

// ErrStagedInputsMismatch is the container reporting a tree that is not the one
// AO staged.
var ErrStagedInputsMismatch = fmt.Errorf("skillrunner: the container did not see the staged inputs")

// sentinelName is written into the staging root and read back from inside the
// container. It is removed immediately afterwards.
const sentinelName = ".ao-visibility-probe"

// visibilityAttempts and visibilityRetryDelay bound the wait for a VM-backed
// filesystem share to catch up. Small on purpose: propagation is sub-second
// when it works at all, and a host that does not share the path should be told
// so quickly rather than after a long hang.
const (
	visibilityAttempts   = 3
	visibilityRetryDelay = 400 * time.Millisecond
)

// ValidateStagingRoot refuses a root AO must not stage into, before it is used.
//
// Every check here is fail-closed, and each covers a way a staging root can be
// wrong in a manner the later steps would not catch cleanly:
//
//   - not absolute, or escaping via "..": a relative or traversing root is a
//     path AO cannot reason about, and the mount would land somewhere nobody
//     chose.
//   - a symlink: the mount would follow it out of the tree an operator
//     reviewed, which is the same class of problem Stage refuses symlinks for.
//   - world-writable: anyone on the host could drop a file into the tree
//     between staging and launch, and the container would scan it as if AO had
//     put it there.
//   - inside AO's own data dir: the run would get a view of AO's database and
//     credentials, which is the one directory it must never see.
func ValidateStagingRoot(root, dataDir string) error {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return fmt.Errorf("%w: no staging root was resolved", ErrStagingUnusable)
	}
	if !filepath.IsAbs(trimmed) {
		return fmt.Errorf("%w: staging root %q must be absolute", ErrStagingUnusable, trimmed)
	}
	clean := filepath.Clean(trimmed)
	if strings.Contains(clean, "..") {
		return fmt.Errorf("%w: staging root %q escapes its own path", ErrStagingUnusable, root)
	}

	// AO's data dir holds the database and every credential in it. A staging
	// root inside it would hand the container a view of both.
	if d := strings.TrimSpace(dataDir); d != "" {
		if within(clean, filepath.Clean(d)) {
			return fmt.Errorf("%w: staging root %q is inside AO's data directory %q; "+
				"a run must never be able to see it", ErrStagingUnusable, clean, d)
		}
	}

	// The parent must exist: AO creates the staging root itself, but not an
	// arbitrary tree above it, and silently doing so would put somebody's
	// source in a directory nobody expected.
	parent := filepath.Dir(clean)
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("%w: staging root's parent %q is not usable: %v", ErrStagingUnusable, parent, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: staging root's parent %q is a symlink; "+
			"AO will not stage through one", ErrStagingUnusable, parent)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: staging root's parent %q is not a directory", ErrStagingUnusable, parent)
	}
	if info.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w: staging root's parent %q is world-writable (%v); "+
			"anything on this host could add a file the container would scan",
			ErrStagingUnusable, parent, info.Mode().Perm())
	}

	// If the root itself already exists it must be a real directory, not a
	// symlink somebody replaced it with between runs.
	if rootInfo, err := os.Lstat(clean); err == nil {
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: staging root %q is a symlink", ErrStagingUnusable, clean)
		}
		if !rootInfo.IsDir() {
			return fmt.Errorf("%w: staging root %q exists and is not a directory", ErrStagingUnusable, clean)
		}
	}
	return nil
}

// within reports whether child is at or below parent, on cleaned paths.
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// VerifyStagingVisible proves, through the runtime, that a container can read
// this host directory.
//
// It writes a sentinel with random content, mounts the root read-only into a
// container built from an image the trust root already approved, and requires
// the exact bytes back. Anything else — an empty mount, a stale tree, a runtime
// that will not start — is a refusal naming the share, because that is the
// thing an operator has to go and change.
//
// The sentinel is removed whether or not the probe succeeds.
func (r *Runner) VerifyStagingVisible(ctx context.Context, root string, image ApprovedImage) error {
	if !r.Available() {
		return fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("%w: cannot create staging root %q: %v", ErrStagingUnusable, root, err)
	}

	token := randomToken()
	sentinel := filepath.Join(root, sentinelName)
	// 0644, not 0600. The probe container runs as nobody (65534) and the file
	// is owned by the daemon's uid, so a 0600 sentinel is unreadable inside and
	// the probe reports "empty" -- diagnosing a share problem that does not
	// exist. The content is a random token that lives for one container start
	// and is removed immediately; there is nothing here to protect.
	if err := os.WriteFile(sentinel, []byte(token), 0o644); err != nil {
		return fmt.Errorf("%w: cannot write the visibility probe into %q: %v", ErrStagingUnusable, root, err)
	}
	defer func() { _ = os.Remove(sentinel) }()

	// Bounded retry, and the reason is specific rather than defensive.
	//
	// A VM-backed runtime shares the host filesystem through virtiofs (or
	// similar), and propagation of a file written microseconds ago is not
	// instantaneous — especially right after the directory was emptied by a
	// previous run's cleanup. Measured on Colima: the first probe answers, one
	// immediately following a cleanup reads nothing, and the next answers
	// again. A single attempt therefore reports "this path is not shared" about
	// a path that is.
	//
	// Retrying cannot mask the failure this exists to catch: a path the runtime
	// genuinely does not share never becomes visible, so every attempt reads
	// empty and the refusal still stands. The only cost is a second or two on a
	// host that is actually broken.
	var got string
	for attempt := 0; attempt < visibilityAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(visibilityRetryDelay):
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		// The same confinement a real run gets. The probe is not a reason to
		// relax anything: no network, non-root, read-only rootfs, no caps.
		out, err := r.runner.Output(probeCtx, r.runtime.Binary,
			"run", "--rm", "--label", RunLabel+"=1",
			"--pull=never", "--network", "none", "--user", nobodyUser,
			"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"-v", root+":/probe:ro", "-w", "/probe",
			image.Ref(), "sh", "-c", "cat /probe/"+sentinelName+" 2>/dev/null || true")
		cancel()
		if err != nil {
			return fmt.Errorf("%w: %q: the probe container did not run: %v", ErrStagingNotVisible, root, err)
		}
		got = strings.TrimSpace(string(out))
		if got == token {
			return nil
		}
	}

	if got == "" {
		// The exact shape of the Colima incident: the mount succeeds and is
		// empty, because the VM never had the path.
		return fmt.Errorf("%w: %q mounted as an EMPTY directory. "+
			"The runtime (%s) accepted the mount and the container saw nothing, which is what "+
			"happens when a VM-backed runtime does not share this host path. Share it with the "+
			"runtime, or point AO_SKILL_STAGING_ROOT at a directory the runtime already shares",
			ErrStagingNotVisible, root, r.runtime.Describe())
	}
	return fmt.Errorf("%w: %q returned unexpected content through the runtime; "+
		"the mount is not showing the bytes AO wrote", ErrStagingNotVisible, root)
}

// stagedInputsDigest is AO's fingerprint of what it staged.
//
// Sorted (path, size, sha256) triples, hashed. Sorted because directory order
// is not a fact about the tree; size and content because a path list alone
// would match a mount carrying different bytes under the same names.
func stagedInputsDigest(inputs []StagedInput) string {
	type row struct {
		path, hash string
		size       int64
	}
	rows := make([]row, 0, len(inputs))
	for _, in := range inputs {
		rows = append(rows, row{path: in.RelPath, hash: in.SHA256, size: in.Bytes})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })

	h := sha256.New()
	for _, rw := range rows {
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", rw.path, rw.size, rw.hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// verifyStagedInputsDelivered compares what AO staged against what the
// container reported seeing.
//
// Three distinct failures, kept distinct because they send an operator to
// different places:
//
//   - an empty mount, which is the Colima shape: the runtime accepted the
//     bind and delivered nothing;
//   - no digest at all, which means the container did not run the accounting
//     AO relies on, so there is nothing to compare and the run cannot be
//     attested;
//   - a digest mismatch, which means the mount carried a tree that is not the
//     one AO prepared.
//
// A count check alone would miss the third and misdiagnose the first.
func verifyStagedInputsDelivered(staging Staging, ev BoundaryEvidence) error {
	want := stagedInputsDigest(staging.Inputs)

	if len(staging.Inputs) > 0 && ev.InputFilesVisible == 0 {
		return fmt.Errorf("%w: AO staged %d file(s) into %q and the container saw none. "+
			"The runtime accepted the mount and delivered an empty directory, which is what a "+
			"VM-backed runtime does for a host path it does not share",
			ErrStagedInputsMismatch, len(staging.Inputs), staging.Dir)
	}
	if ev.InputDigest == "" {
		return fmt.Errorf("%w: the container reported no input digest, so what it read cannot be "+
			"checked against what AO staged", ErrStagedInputsMismatch)
	}
	if ev.InputDigest != want {
		return fmt.Errorf("%w: AO staged %d file(s) fingerprinted %s; the container reported %s. "+
			"The mount did not deliver the tree AO prepared, and a scan over a different tree is "+
			"not a scan of this project",
			ErrStagedInputsMismatch, len(staging.Inputs), short(want), short(ev.InputDigest))
	}
	return nil
}

// short trims a digest for a message. The full value is not the useful part
// when the point is that two of them differ.
func short(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}
