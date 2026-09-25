package skillrunner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// cleanup.go — removing AO's containers, and knowing whether it happened.
//
// Before this file, teardown was `docker rm -f` with its error ignored, and a
// reaped container was counted as removed because the command had been issued.
// Neither is evidence: `docker rm -f` exits 0 for a container that does not
// exist, and a runtime that is wedged answers nothing at all. So:
//
//   - removal is CONFIRMED by asking the runtime afterwards whether the container
//     is still there. Only an answer of "absent" counts;
//   - anything else is PENDING: recorded, reported, and retried by SweepOwned
//     once the runtime answers again. AO never says it removed a container it
//     cannot see gone;
//   - what may be removed is decided by ownership AO can prove: the exact name
//     AO generated for a container it started, the run id of a run of AO's, or
//     this installation's OwnerLabel. A container without one of those is not
//     AO's to touch, whatever else it carries.

// OwnerLabel carries the AO installation id that started a container. It is
// what lets a sweep after a restart find this installation's leftovers -- and
// ONLY this installation's: another AO on the same runtime has a different id,
// and a container without the label (another tool, or an AO build that predates
// it) is never swept.
const OwnerLabel = "ao.skillrun.owner"

// KindLabel says what a container was for: "run" for a skill run, "probe" for
// a check AO makes of the runtime itself (staging visibility, egress).
const KindLabel = "ao.skillrun.kind"

const (
	kindRun   = "run"
	kindProbe = "probe"
)

// ErrCleanupPending is a container AO started whose removal the runtime did
// not confirm. It is recorded and retried; it is never reported as removed.
var ErrCleanupPending = errors.New("skillrunner: container removal is pending")

// CleanupStatus is what AO knows about a container after trying to remove it.
type CleanupStatus string

const (
	// CleanupConfirmed means the runtime reported the container absent after
	// the removal.
	CleanupConfirmed CleanupStatus = "confirmed"
	// CleanupPending means the runtime did not confirm it: it is recorded and
	// SweepOwned retries it.
	CleanupPending CleanupStatus = "pending"
)

// PendingContainer is one container whose removal is not confirmed.
type PendingContainer struct {
	// Ref is the container's name (for containers AO named) or id (for ones
	// found by label).
	Ref string
	// RunID is the durable run it belongs to, when there is one.
	RunID string
	// Since is when the first removal attempt failed to confirm.
	Since time.Time
	// LastError is why the last attempt did not confirm.
	LastError string
}

// cleanupState is the Runner's record of containers in flight and removals
// pending. It is in memory; after a restart the durable record is the
// container's own labels, which SweepOwned reads.
type cleanupState struct {
	mu       sync.Mutex
	inflight map[string]string // container name -> run id
	pending  map[string]PendingContainer
}

// WithOwner records the AO installation id this runner labels its containers
// with. Without it containers carry no OwnerLabel and SweepOwned sweeps
// nothing by label: ownership AO cannot prove is ownership it does not claim.
func (r *Runner) WithOwner(installationID string) *Runner {
	r.owner = strings.TrimSpace(installationID)
	return r
}

// ownerArgs are the labels every container AO starts carries. They grant
// nothing -- isolation is decided by the fixed flags in containerArgs -- and
// exist so AO can find, and only find, its own.
func (r *Runner) ownerArgs(kind, runID string) []string {
	args := []string{"--label", RunLabel + "=1", "--label", KindLabel + "=" + kind}
	if r.owner != "" {
		args = append(args, "--label", OwnerLabel+"="+r.owner)
	}
	if runID != "" {
		args = append(args, "--label", RunIDLabel+"="+runID)
	}
	return args
}

// track marks a container name as in flight, so a concurrent sweep does not
// remove a container that is still doing its job.
func (r *Runner) track(name, runID string) {
	r.cleanup.mu.Lock()
	defer r.cleanup.mu.Unlock()
	if r.cleanup.inflight == nil {
		r.cleanup.inflight = map[string]string{}
	}
	r.cleanup.inflight[name] = runID
}

func (r *Runner) untrack(name string) {
	r.cleanup.mu.Lock()
	defer r.cleanup.mu.Unlock()
	delete(r.cleanup.inflight, name)
}

func (r *Runner) isInflight(name string) bool {
	r.cleanup.mu.Lock()
	defer r.cleanup.mu.Unlock()
	_, ok := r.cleanup.inflight[name]
	return ok
}

// removeContainer force-removes one container AO started and confirms it is
// gone. ref is the name AO gave it or the id the runtime listed. Both the
// removal and the confirmation are bounded by probeTimeout each, whatever ctx
// allows, so teardown can never hold a worker longer than that.
func (r *Runner) removeContainer(ctx context.Context, ref, runID string) CleanupStatus {
	defer r.untrack(ref)

	rmCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	_, rmErr := r.runner.Output(rmCtx, r.runtime.Binary, "rm", "-f", ref)
	cancel()

	// `rm -f` exits 0 for a container that does not exist, and says nothing
	// useful when the runtime is wedged. Whether it is gone is a question for
	// the runtime, asked separately.
	absent, psErr := r.containerAbsent(ctx, ref)
	if psErr == nil && absent {
		r.cleanup.mu.Lock()
		delete(r.cleanup.pending, ref)
		r.cleanup.mu.Unlock()
		return CleanupConfirmed
	}

	why := "the runtime still lists it"
	switch {
	case psErr != nil:
		why = "the runtime did not answer whether it is gone: " + psErr.Error()
	case rmErr != nil:
		why = "remove failed and the runtime still lists it: " + rmErr.Error()
	}
	r.cleanup.mu.Lock()
	defer r.cleanup.mu.Unlock()
	if r.cleanup.pending == nil {
		r.cleanup.pending = map[string]PendingContainer{}
	}
	p, seen := r.cleanup.pending[ref]
	if !seen {
		p = PendingContainer{Ref: ref, RunID: runID, Since: time.Now().UTC()}
	}
	p.LastError = truncateMessage(why, 400)
	r.cleanup.pending[ref] = p
	return CleanupPending
}

// containerAbsent asks the runtime whether a container still exists, by exact
// name or by id. An error means the runtime did not answer, which is NOT
// "absent".
func (r *Runner) containerAbsent(ctx context.Context, ref string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	filter := "id=" + ref
	if strings.HasPrefix(ref, "ao-") {
		// A name AO generated. The runtime stores names with a leading slash
		// and filters by regex, so anchor both ends: "ao-x" must not match
		// "ao-x2".
		filter = "name=^/" + ref + "$"
	}
	out, err := r.runner.Output(ctx, r.runtime.Binary, "ps", "-aq", "--no-trunc", "--filter", filter)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}

// PendingCleanup lists the containers whose removal AO has not confirmed, in a
// stable order.
func (r *Runner) PendingCleanup() []PendingContainer {
	r.cleanup.mu.Lock()
	defer r.cleanup.mu.Unlock()
	out := make([]PendingContainer, 0, len(r.cleanup.pending))
	for _, p := range r.cleanup.pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// PendingCleanupFor reports whether any container of runID is pending removal.
func (r *Runner) PendingCleanupFor(runID string) bool {
	if runID == "" {
		return false
	}
	r.cleanup.mu.Lock()
	defer r.cleanup.mu.Unlock()
	for _, p := range r.cleanup.pending {
		if p.RunID == runID {
			return true
		}
	}
	return false
}

// SweepReport says what SweepOwned did.
type SweepReport struct {
	// Removed are containers whose removal the runtime confirmed.
	Removed []string
	// Pending are containers still not confirmed gone.
	Pending []string
	// NetworksRemoved are networks whose removal the runtime confirmed.
	NetworksRemoved []string
	// NetworksPending are networks still not confirmed gone.
	NetworksPending []string
}

// SweepOwned retries every pending removal and removes this installation's
// leftover containers: those carrying OwnerLabel for this installation that
// are not in flight here and whose run, if they belong to one, is not live.
//
// live answers whether a run is still executing in THIS daemon; its
// containers are left alone. A container is never swept because of a label
// alone unless that label is this installation's owner id, and never at all
// when this runner has no owner id.
func (r *Runner) SweepOwned(ctx context.Context, live func(runID string) bool) (SweepReport, error) {
	var rep SweepReport
	if !r.Available() {
		return rep, nil
	}
	type candidate struct{ ref, runID string }
	candidates := map[string]candidate{}
	for _, p := range r.PendingCleanup() {
		candidates[p.Ref] = candidate{ref: p.Ref, runID: p.RunID}
	}

	var listErr error
	if r.owner != "" {
		listCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		out, err := r.runner.Output(listCtx, r.runtime.Binary, "ps", "-a",
			"--filter", "label="+OwnerLabel+"="+r.owner,
			"--format", "{{.Names}}\t{{.Label \""+RunIDLabel+"\"}}")
		cancel()
		if err != nil {
			listErr = fmt.Errorf("skillrunner: list this installation's containers: %w", err)
		} else {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				name, runID, _ := strings.Cut(strings.TrimSpace(line), "\t")
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				candidates[name] = candidate{ref: name, runID: strings.TrimSpace(runID)}
			}
		}
	}

	refs := make([]string, 0, len(candidates))
	for ref := range candidates {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		c := candidates[ref]
		if r.isInflight(c.ref) || (c.runID != "" && live != nil && live(c.runID)) {
			continue
		}
		if ctx.Err() != nil {
			rep.Pending = append(rep.Pending, c.ref)
			continue
		}
		if r.removeContainer(ctx, c.ref, c.runID) == CleanupConfirmed {
			rep.Removed = append(rep.Removed, c.ref)
		} else {
			rep.Pending = append(rep.Pending, c.ref)
		}
	}

	// Networks: a pentest run leaves an internal and an egress network behind
	// on a crash. They carry this installation's owner label precisely so a
	// sweep can find them; reclaim any whose run is not still live here,
	// confirmed gone. The containers were swept just above, so a reclaimable
	// network is normally already empty; removeNetworkConfirmed force-detaches
	// anything still on it. A network is never swept on the owner label alone
	// while its run is live, and never at all when this runner has no owner id.
	if r.owner != "" && ctx.Err() == nil {
		nlCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		nout, nerr := r.runner.Output(nlCtx, r.runtime.Binary, "network", "ls",
			"--filter", "label="+OwnerLabel+"="+r.owner,
			"--format", "{{.Name}}\t{{.Label \""+RunIDLabel+"\"}}")
		cancel()
		if nerr != nil {
			if listErr == nil {
				listErr = fmt.Errorf("skillrunner: list this installation's networks: %w", nerr)
			}
		} else {
			type netcand struct{ name, runID string }
			nets := make([]netcand, 0)
			for _, line := range strings.Split(strings.TrimSpace(string(nout)), "\n") {
				name, runID, _ := strings.Cut(strings.TrimSpace(line), "\t")
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				nets = append(nets, netcand{name: name, runID: strings.TrimSpace(runID)})
			}
			sort.Slice(nets, func(i, j int) bool { return nets[i].name < nets[j].name })
			for _, n := range nets {
				if n.runID != "" && live != nil && live(n.runID) {
					continue
				}
				if ctx.Err() != nil {
					rep.NetworksPending = append(rep.NetworksPending, n.name)
					continue
				}
				if r.removeNetworkConfirmed(ctx, n.name) == CleanupConfirmed {
					rep.NetworksRemoved = append(rep.NetworksRemoved, n.name)
				} else {
					rep.NetworksPending = append(rep.NetworksPending, n.name)
				}
			}
		}
	}

	if listErr != nil {
		return rep, listErr
	}
	if leftover := append(append([]string{}, rep.Pending...), rep.NetworksPending...); len(leftover) > 0 {
		return rep, fmt.Errorf("%w: %s", ErrCleanupPending, strings.Join(leftover, ", "))
	}
	return rep, nil
}

// truncateMessage bounds a diagnostic that ends up in logs and API answers.
func truncateMessage(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
