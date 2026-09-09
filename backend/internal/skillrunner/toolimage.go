package skillrunner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// toolimage.go — AO chooses what runs, not the skill.
//
// A manifest names a TOOL from a closed vocabulary; AO maps that tool to a
// base image and to a command AO itself authors. A manifest cannot name an
// image, cannot name a binary, and cannot contribute a single argument to the
// command line. That is the difference between "the package declares what it
// wants" and "the package decides what executes as us".
//
// This is why arbitrary_process_execution is a separate control that nothing
// here attests: the closed contract below is deliberately not general.

// ErrToolNotApproved is returned for any tool AO does not ship a contract for.
var ErrToolNotApproved = errors.New("skillrunner: tool is not in AO's approved set")

// Tool is one closed execution contract.
type Tool string

const (
	// ToolStaticScan is a read-only pattern scan over a staged checkout. It
	// runs one AO-authored script, reads only what AO mounted, and opens no
	// network. It is not a SAST engine and its report says so.
	ToolStaticScan Tool = "ao.static-scan/v1"
)

// ToolContract binds a tool to the exact image and command AO will run.
type ToolContract struct {
	Tool Tool
	// BaseImage is the repository the digest belongs to. It is recorded so a
	// mismatch between a configured digest and its repository is visible.
	BaseImage string
	// Digest pins the image content. AO refuses a tag anywhere: a tag is a
	// mutable pointer and "the image we tested" has to mean one thing.
	Digest string
	// Argv is authored here, in Go, and takes nothing from the manifest.
	Argv func(params ToolParams) []string
	// Description is what an approver reads.
	Description string
}

// ToolParams are the only values a skill influences, and each is validated
// before it reaches a command line.
type ToolParams struct {
	// MaxFiles bounds how many files the scan will read.
	MaxFiles int
	// MaxFileBytes skips any file larger than this — a minified bundle or a
	// vendored blob is not source worth scanning, and reading one would spend
	// the whole budget.
	MaxFileBytes int
}

// DefaultToolParams are conservative on purpose. A scan that needs more should
// say so and have somebody agree.
func DefaultToolParams() ToolParams {
	return ToolParams{MaxFiles: 2000, MaxFileBytes: 512 << 10}
}

// Validate bounds the params. They reach a shell as numbers, so "is this an
// integer in range" is the whole of what must be true.
func (p ToolParams) Validate() error {
	if p.MaxFiles < 1 || p.MaxFiles > 100000 {
		return fmt.Errorf("skillrunner: maxFiles %d is out of range (1..100000)", p.MaxFiles)
	}
	if p.MaxFileBytes < 1024 || p.MaxFileBytes > (64<<20) {
		return fmt.Errorf("skillrunner: maxFileBytes %d is out of range (1KiB..64MiB)", p.MaxFileBytes)
	}
	return nil
}

// digestRe is the exact shape a pinned digest must have.
var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// approvedTools is AO's whole execution vocabulary. Adding an entry is a code
// change and a release — which is the point, because it is the moment somebody
// decides a new thing may run as AO.
//
// The digest is empty here and comes from an ADMINISTRATOR'S APPROVAL at
// execution time (see imagetrust.go), not from whatever the host happens to
// hold. AO ships no registry credentials and pulls nothing: an image that is
// not already on the host makes the tool unavailable, which is a refusal rather
// than a silent network fetch.
var approvedTools = map[Tool]ToolContract{
	ToolStaticScan: {
		Tool:        ToolStaticScan,
		BaseImage:   "alpine:3.19",
		Argv:        staticScanArgv,
		Description: "Read-only pattern scan over a staged checkout. No network, no writes.",
	},
}

// ApprovedTools lists the vocabulary in a stable order.
func ApprovedTools() []Tool {
	out := make([]Tool, 0, len(approvedTools))
	for t := range approvedTools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// resolveProbeContract binds a tool to whatever image the host currently has
// under the base name.
//
// It is UNEXPORTED, and it is not an execution path. It exists for one caller:
// the egress boundary probe, which needs some container to observe a network
// refusal from. That container runs an AO-authored `nc` loop over no inputs and
// produces no report, so "whichever alpine is on this host" is an acceptable
// answer there.
//
// It is deliberately NOT how a skill runs. Trusting the image that happens to
// be present trusts whoever last ran `docker pull`; a skill execution goes
// through ResolveApprovedImage, which requires an administrator's recorded
// decision about an exact digest. Re-exporting this, or calling it from a run
// path, would put the trust root back where it was.
func (r *Runner) resolveProbeContract(ctx context.Context, tool Tool) (ToolContract, error) {
	contract, ok := approvedTools[tool]
	if !ok {
		return ToolContract{}, fmt.Errorf("%w: %q (approved: %s)",
			ErrToolNotApproved, tool, joinTools(ApprovedTools()))
	}
	if !r.Available() {
		return ToolContract{}, fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := r.runner.Output(ctx, r.runtime.Binary, "image", "inspect", contract.BaseImage, "--format", "{{.Id}}")
	if err != nil {
		return ToolContract{}, fmt.Errorf("%w: base image %s is not present on this host, "+
			"and AO does not pull: %v", ErrRuntimeUnavailable, contract.BaseImage, err)
	}
	digest := strings.TrimSpace(string(out))
	if !digestRe.MatchString(digest) {
		return ToolContract{}, fmt.Errorf("%w: %s resolved to %q, which is not a content digest",
			ErrRuntimeUnavailable, contract.BaseImage, digest)
	}
	contract.Digest = digest
	return contract, nil
}

// PinnedRef is the digest-pinned reference AO passes to the runtime.
func (c ToolContract) PinnedRef() string {
	repo, _, _ := strings.Cut(c.BaseImage, ":")
	return repo + "@" + c.Digest
}

func joinTools(tools []Tool) string {
	parts := make([]string, 0, len(tools))
	for _, t := range tools {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
}
