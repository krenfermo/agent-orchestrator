package skillrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
)

// imagetrust.go — AO runs the image it was authorized to run, and no other.
//
// # What changed, and why it had to
//
// Until this file, ResolveContract asked the runtime what `alpine:3.19` was and
// used the answer. That trusts whoever last ran `docker pull` on this machine.
// It is the "confianza implícita en imágenes presentes en el host" the trust-root
// decision rules out, and it is not a small gap: a compromised base image nobody
// reviewed would have executed as AO, with AO's staging mounted into it.
//
// Now an execution needs an APPROVAL — a named administrator's recorded decision
// that this exact scope may run this exact digest to back this exact tool — and
// the digest is checked against what the runtime actually holds, twice:
//
//  1. Before launch. `image inspect <digest>` must come back with the same
//     digest. An absent image is a refusal; AO passes --pull=never and has no
//     code path that pulls, builds or updates anything.
//  2. Immediately before launch, again, against the authority. This is the
//     revocation window: an approval withdrawn between planning and launching
//     stops the launch.
//
// # What revocation does and does not do
//
// It stops NEW executions, at the last possible moment before the container
// starts. It does not kill a container that is already running, and this file
// does not pretend otherwise — see RevocationPolicy below, which is written
// down precisely because the alternative is somebody assuming a guarantee that
// does not exist.

// ErrImageNotApproved is returned when nothing authorizes the image AO was
// about to run. Every distinct cause — no approval, expired, revoked, wrong
// scope, wrong tool, wrong digest — resolves to this, because from the caller's
// side they are one condition: do not run.
var ErrImageNotApproved = errors.New("skillrunner: no active approval authorizes this image")

// ImageAuthority answers "which image may this scope run".
//
// It is an interface so the runner depends on the DECISION and not on the
// database, and so this package's tests can drive every refusal without a
// store. The implementation lives in service/skills, over migration 0163.
type ImageAuthority interface {
	// ApprovedImage returns the active approval for one scope and tool.
	//
	// It must apply expiry and revocation at the moment of the call rather
	// than caching: this is called immediately before launch precisely so a
	// revocation lands.
	ApprovedImage(ctx context.Context, scope skillimage.Scope, tool string) (skillimage.Approval, error)
}

// RevocationPolicy is AO's stated behaviour when an approval is withdrawn. It
// is a documented constant rather than a comment because a caller building a UI
// or an incident runbook needs the exact promise, and the exact non-promise.
const RevocationPolicy = `Revoking an image approval stops new executions immediately: the approval is
re-checked at the last point before the container starts, so a launch already in
flight is refused rather than started.

It does NOT stop a container that is already running. AO does not kill running
skill containers on revocation. Two reasons, both deliberate: a kill mid-run
produces a truncated report that a reader could mistake for a completed one, and
the blast radius is already bounded -- a static-code run has no network, a
read-only mount and a wall clock measured in minutes, so "wait for it to finish"
is a bounded wait. A run that was launched under an approval revoked afterwards
is recorded as such in its report and in the audit trail, so the fact is
answerable rather than invisible.

It does NOT recall secrets already delivered. A value handed to a container is
in that container's memory, and no approval state can reach in and remove it.
Revocation stops the NEXT delivery. Treat any secret a revoked run received as
exposed to that run for its lifetime, and rotate it if that matters.

An operator who needs a running container stopped now should stop it by hand;
the label ao.skillrun=1 identifies exactly AO's skill runs and nothing else.`

// ApprovedImage is one authorization, resolved and verified against this host.
type ApprovedImage struct {
	// Approval is what authorized it, kept whole so the report and the audit
	// line can name the approver and the scope rather than only the digest.
	Approval skillimage.Approval
	// Digest is what will be run. It is the approval's digest, confirmed to be
	// what the runtime actually holds under that identity.
	Digest string
	// Contract is the AO-authored command this image backs.
	Contract ToolContract
}

// Ref is what AO passes to the runtime: the bare digest, never a name.
//
// A name would reintroduce the mutable pointer the approval exists to remove —
// `alpine:3.19` resolves to whatever the host currently calls that, and the
// whole point is that it resolves to nothing at all.
func (a ApprovedImage) Ref() string { return a.Digest }

// ResolveApprovedImage binds a tool to the image this scope is authorized to
// run, and proves the host actually holds it.
//
// Every failure is a refusal. There is no path here that pulls, builds, falls
// back to a tag, or reaches for "the image that is probably right".
func (r *Runner) ResolveApprovedImage(
	ctx context.Context, authority ImageAuthority, scope skillimage.Scope, tool Tool,
) (ApprovedImage, error) {
	contract, ok := approvedTools[tool]
	if !ok {
		return ApprovedImage{}, fmt.Errorf("%w: %q (approved: %s)",
			ErrToolNotApproved, tool, joinTools(ApprovedTools()))
	}
	if !r.Available() {
		return ApprovedImage{}, fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
	}
	if authority == nil {
		// A nil authority is not "allow everything"; it is an installation with
		// no trust root, which can authorize nothing.
		return ApprovedImage{}, fmt.Errorf("%w: this installation has no image trust root configured",
			ErrImageNotApproved)
	}
	if err := scope.Validate(); err != nil {
		return ApprovedImage{}, err
	}

	approval, err := authority.ApprovedImage(ctx, scope, string(tool))
	if err != nil {
		return ApprovedImage{}, err
	}
	if approval.Tool != string(tool) {
		return ApprovedImage{}, fmt.Errorf("%w: the approval backs %q, not %q",
			ErrImageNotApproved, approval.Tool, tool)
	}
	if !approval.Scope.Matches(scope) {
		// A store bug or a mis-wired caller, not a user error. It is checked
		// here anyway: the authority is trusted to apply the scope, and a
		// second check costs nothing and turns a silent mismatch into a refusal.
		return ApprovedImage{}, fmt.Errorf("%w: the approval covers %s, not %s",
			ErrImageNotApproved, approval.Scope, scope)
	}
	digest, err := skillimage.ParseDigest(approval.Digest)
	if err != nil {
		return ApprovedImage{}, fmt.Errorf("%w: %w", ErrImageNotApproved, err)
	}

	// The host must actually hold these bytes under this identity. Resolution
	// is BY DIGEST: no tag is consulted, so nothing the host has retagged can
	// answer for the approved image.
	effective, err := r.effectiveDigest(ctx, digest)
	if err != nil {
		return ApprovedImage{}, err
	}
	if effective != digest {
		return ApprovedImage{}, fmt.Errorf(
			"%w: the runtime resolves %s to %s; AO runs the digest that was approved and nothing else",
			ErrImageNotApproved, digest, effective)
	}

	contract.Digest = digest
	contract.BaseImage = approval.Reference
	return ApprovedImage{Approval: approval, Digest: digest, Contract: contract}, nil
}

// effectiveDigest asks the runtime what it actually holds under one digest.
//
// This is the check the previous phase did not have: BoundaryEvidence recorded
// the reference AO PASSED, which is an echo and proves nothing. What matters is
// what the runtime resolved, and it is read from the runtime.
func (r *Runner) effectiveDigest(ctx context.Context, digest string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := r.runner.Output(ctx, r.runtime.Binary, "image", "inspect", digest, "--format", "{{.Id}}")
	if err != nil {
		return "", fmt.Errorf("%w: %s is not present on this host, and AO does not pull: %v",
			ErrImageNotApproved, digest, err)
	}
	effective := strings.TrimSpace(string(out))
	if _, parseErr := skillimage.ParseDigest(effective); parseErr != nil {
		return "", fmt.Errorf("%w: the runtime answered %q for %s, which is not a digest",
			ErrImageNotApproved, effective, digest)
	}
	return effective, nil
}

// RecheckBeforeLaunch re-asks the authority immediately before the container
// starts, and refuses if anything changed.
//
// It exists as a separate call, at a separate moment, on purpose. Resolution
// happens while a plan is assembled — staging a checkout takes seconds, and an
// approval revoked during those seconds must not be honoured. This is the last
// point at which "may AO run this" is asked, and the answer is used within
// microseconds of being given.
func (r *Runner) RecheckBeforeLaunch(
	ctx context.Context, authority ImageAuthority, image ApprovedImage,
) error {
	if authority == nil {
		return fmt.Errorf("%w: this installation has no image trust root configured", ErrImageNotApproved)
	}
	again, err := authority.ApprovedImage(ctx, image.Approval.Scope, image.Approval.Tool)
	if err != nil {
		return fmt.Errorf("%w: the approval did not survive to launch: %w", ErrImageNotApproved, err)
	}
	if again.Digest != image.Digest {
		return fmt.Errorf("%w: the approval for %s changed to %s between resolution and launch",
			ErrImageNotApproved, image.Digest, again.Digest)
	}
	if again.ID != image.Approval.ID {
		return fmt.Errorf("%w: approval %s was replaced by %s between resolution and launch",
			ErrImageNotApproved, image.Approval.ID, again.ID)
	}
	return nil
}

// RevokedSince reports whether the approval that authorized a run has stopped
// being active since it started.
//
// It is asked AFTER a run completes, and its answer goes into the report. AO
// does not kill the container — see RevocationPolicy — but a reader of a report
// produced under a withdrawn approval has to be able to see that, and a silent
// omission would be the worst of both.
func (r *Runner) RevokedSince(
	ctx context.Context, authority ImageAuthority, image ApprovedImage, now time.Time,
) bool {
	if authority == nil {
		return true
	}
	again, err := authority.ApprovedImage(ctx, image.Approval.Scope, image.Approval.Tool)
	if err != nil {
		return true
	}
	return again.Digest != image.Digest || !again.Active(now)
}
