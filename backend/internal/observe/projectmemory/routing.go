package projectmemory

import (
	stdctx "context"
	"fmt"
	"strings"
)

// RoutingSchemaVersion identifies the shape of the routing block an evidence
// record carries. It is versioned separately from EvidenceSchemaVersion
// because the routing block is an ADDITIVE extension: a v1 baseline record
// written before the context router existed and one written after it are the
// same schema, and a consumer that knows nothing about routing keeps reading
// both. Only a consumer that opts into the block needs to know this version.
const RoutingSchemaVersion = "context-router-metrics/v1"

// RoutingMetrics is what the role-aware context router decided for one
// dispatch, recorded beside the baseline's existing context measurements
// rather than in place of them.
//
// The question it answers is the one a token-savings claim cannot be made
// without: of everything this dispatch COULD have been sent, how much was
// actually sent, and where did the sent part come from — content AO read
// fresh for this dispatch, or content it had already indexed and merely
// reused?
//
// Every size is recorded twice: a byte figure AO measured, and a token figure
// AO estimated from it. They are separate fields rather than one field with a
// basis flag so a reader never has to discover, halfway through a comparison,
// that two records labeled their sizes differently. The bytes are measured,
// the tokens are estimated, and each metric says so itself.
type RoutingMetrics struct {
	// SchemaVersion is RoutingSchemaVersion, so this block can evolve without
	// touching the record's own version.
	SchemaVersion string `json:"schemaVersion"`
	// Enabled reports whether a router selection actually shaped this
	// dispatch's payload. False covers every way it did not: the feature flag
	// off, no router wired, a routable checkout root that could not be
	// resolved, a selection that failed and fell back to the full payload.
	// Reason states which.
	Enabled bool `json:"enabled"`
	// Reason explains a disabled routing block, and is empty when routing ran.
	Reason string `json:"reason,omitempty"`
	// Role and Tier are the router's own vocabulary for what it assembled:
	// which role budget applied and how deep the retrieval went. Empty when
	// routing did not run.
	Role string `json:"role,omitempty"`
	Tier string `json:"tier,omitempty"`

	// Sections is how many blocks the payload carries, Dropped how many
	// candidates the budget could not hold, and Truncated how many of the sent
	// blocks were cut to fit.
	Sections  int `json:"sections"`
	Dropped   int `json:"dropped"`
	Truncated int `json:"truncated"`

	// PotentialBytes / PotentialTokens size everything the dispatch could have
	// been sent: every candidate the router assembled, at full length, before
	// any budget was applied. With routing disabled it is the unrouted payload
	// itself, which is exactly what that dispatch had available.
	PotentialBytes  Metric `json:"potentialBytes"`
	PotentialTokens Metric `json:"potentialTokens"`
	// SelectedBytes / SelectedTokens size what was actually sent.
	SelectedBytes  Metric `json:"selectedBytes"`
	SelectedTokens Metric `json:"selectedTokens"`
	// ReusedBytes / ReusedTokens size the part of the sent payload that came
	// out of a store AO had already built — the code graph index and the
	// durable project memory. It is the part a dispatch did not have to read
	// the repository to obtain.
	ReusedBytes  Metric `json:"reusedBytes"`
	ReusedTokens Metric `json:"reusedTokens"`
	// NewBytes / NewTokens size the rest: content read for this dispatch (the
	// caller's own documents, the current diff, the task statement).
	NewBytes  Metric `json:"newBytes"`
	NewTokens Metric `json:"newTokens"`

	// LimitTokens is the token target the selection was packed against and
	// HardCapTokens the role's cap, both unavailable when routing did not run.
	LimitTokens   Metric `json:"limitTokens"`
	HardCapTokens Metric `json:"hardCapTokens"`

	// Notes are the router's own operator-facing remarks (a source that
	// failed, an expansion that was skipped). They carry no metrics.
	Notes []string `json:"notes,omitempty"`
}

// MeasuredBytesMethod and EstimatedTokensMethod name how the two halves of
// every routing size were obtained, so the strings are identical across
// records instead of being retyped per call site.
const (
	measuredRoutedBytesMethod   = "utf8 byte length of the routed payload sections"
	measuredUnroutedBytesMethod = "utf8 byte length of the payload AO handed the provider unrouted"
)

// RoutingDisabled builds the routing block for a dispatch the router did not
// shape. reason says why.
//
// payloadBytes is the size of the payload that dispatch sent anyway, and it is
// recorded as both the potential and the selected size: with no router in the
// path, everything available was sent. The reused size is a measured zero
// rather than an unavailable — "nothing was drawn from AO's indexed stores" is
// a fact about a dispatch no router touched, not a gap in the measurement.
func RoutingDisabled(payloadBytes int64, reason string) RoutingMetrics {
	if payloadBytes < 0 {
		payloadBytes = 0
	}
	if strings.TrimSpace(reason) == "" {
		reason = "no context router selection was applied to this dispatch"
	}
	noRouter := "the context router did not shape this dispatch, so nothing was drawn from AO's indexed stores"
	return RoutingMetrics{
		SchemaVersion:   RoutingSchemaVersion,
		Enabled:         false,
		Reason:          reason,
		PotentialBytes:  Measured(payloadBytes, measuredUnroutedBytesMethod),
		PotentialTokens: EstimatedTokensFor(payloadBytes),
		SelectedBytes:   Measured(payloadBytes, measuredUnroutedBytesMethod),
		SelectedTokens:  EstimatedTokensFor(payloadBytes),
		ReusedBytes:     Measured(0, noRouter),
		ReusedTokens:    Estimated(0, noRouter),
		NewBytes:        Measured(payloadBytes, measuredUnroutedBytesMethod),
		NewTokens:       EstimatedTokensFor(payloadBytes),
		LimitTokens:     Unavailable("no router budget applied to this dispatch"),
		HardCapTokens:   Unavailable("no router budget applied to this dispatch"),
	}
}

// RoutingSelection is a router selection's measured sizes, handed over for
// recording. The caller supplies byte totals; the token figures are derived by
// RoutingSelected, so every routing record estimates tokens the same way.
//
// It exists so the contextrouter package can hand its Selection over without
// this package having to import it — the dependency runs the other way, since
// the router already sizes its payload with this package's own heuristic.
type RoutingSelection struct {
	Role      string
	Tier      string
	Sections  int
	Dropped   int
	Truncated int
	// PotentialBytes is every candidate at full length, before packing.
	PotentialBytes int64
	// SelectedBytes is the packed payload, ReusedBytes the part of it drawn
	// from AO's indexed stores and NewBytes the part read for this dispatch.
	// The last two are expected to sum to SelectedBytes; RoutingSelected does
	// not silently reconcile them if they do not.
	SelectedBytes int64
	ReusedBytes   int64
	NewBytes      int64
	LimitTokens   int
	HardCapTokens int
	Notes         []string
}

// RoutingSelected turns a router selection's measured sizes into the routing
// block an evidence record carries.
func RoutingSelected(sel RoutingSelection) RoutingMetrics {
	indexed := "utf8 byte length of the routed sections built from AO's code graph and project memory"
	read := "utf8 byte length of the routed sections built from content read for this dispatch"
	return RoutingMetrics{
		SchemaVersion:   RoutingSchemaVersion,
		Enabled:         true,
		Role:            sel.Role,
		Tier:            sel.Tier,
		Sections:        sel.Sections,
		Dropped:         sel.Dropped,
		Truncated:       sel.Truncated,
		PotentialBytes:  Measured(nonNegative(sel.PotentialBytes), "utf8 byte length of every candidate section at full length, before the role budget was applied"),
		PotentialTokens: EstimatedTokensFor(nonNegative(sel.PotentialBytes)),
		SelectedBytes:   Measured(nonNegative(sel.SelectedBytes), measuredRoutedBytesMethod),
		SelectedTokens:  EstimatedTokensFor(nonNegative(sel.SelectedBytes)),
		ReusedBytes:     Measured(nonNegative(sel.ReusedBytes), indexed),
		ReusedTokens:    EstimatedTokensFor(nonNegative(sel.ReusedBytes)),
		NewBytes:        Measured(nonNegative(sel.NewBytes), read),
		NewTokens:       EstimatedTokensFor(nonNegative(sel.NewBytes)),
		LimitTokens:     Measured(int64(nonNegativeInt(sel.LimitTokens)), "token target the selection was packed against"),
		HardCapTokens:   Measured(int64(nonNegativeInt(sel.HardCapTokens)), "hard cap of the role budget this selection used"),
		Notes:           sel.Notes,
	}
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func nonNegativeInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// ReductionPercent reports how much smaller the selected payload is than the
// potential one, as a percentage of the potential size, and whether that
// figure could be computed at all.
//
// It returns ok=false rather than zero when either size is unavailable or the
// potential size is zero: "no reduction" and "no basis for a reduction figure"
// are different findings, and a harness that printed 0% for the second would
// be reporting a measurement it never made.
func (r RoutingMetrics) ReductionPercent() (float64, bool) {
	potential, selected := r.PotentialBytes.Value, r.SelectedBytes.Value
	if potential == nil || selected == nil || *potential <= 0 {
		return 0, false
	}
	return float64(*potential-*selected) / float64(*potential) * 100, true
}

// normalized fills in the block's schema version and turns never-populated
// metrics into explicit "not recorded" entries, exactly as the record itself
// does. It fills in absences, never numbers.
func (r RoutingMetrics) normalized() RoutingMetrics {
	if r.SchemaVersion == "" {
		r.SchemaVersion = RoutingSchemaVersion
	}
	r.PotentialBytes = r.PotentialBytes.normalized()
	r.PotentialTokens = r.PotentialTokens.normalized()
	r.SelectedBytes = r.SelectedBytes.normalized()
	r.SelectedTokens = r.SelectedTokens.normalized()
	r.ReusedBytes = r.ReusedBytes.normalized()
	r.ReusedTokens = r.ReusedTokens.normalized()
	r.NewBytes = r.NewBytes.normalized()
	r.NewTokens = r.NewTokens.normalized()
	r.LimitTokens = r.LimitTokens.normalized()
	r.HardCapTokens = r.HardCapTokens.normalized()
	return r
}

// metrics names every metric in the block for the record's own validation
// pass, so a routing figure is held to the same labeling rule as a baseline
// one.
func (r RoutingMetrics) metrics() []namedMetric {
	return []namedMetric{
		{"routing.potentialBytes", r.PotentialBytes},
		{"routing.potentialTokens", r.PotentialTokens},
		{"routing.selectedBytes", r.SelectedBytes},
		{"routing.selectedTokens", r.SelectedTokens},
		{"routing.reusedBytes", r.ReusedBytes},
		{"routing.reusedTokens", r.ReusedTokens},
		{"routing.newBytes", r.NewBytes},
		{"routing.newTokens", r.NewTokens},
		{"routing.limitTokens", r.LimitTokens},
		{"routing.hardCapTokens", r.HardCapTokens},
	}
}

// Validate checks the block's own required fields on top of its metrics.
func (r RoutingMetrics) Validate() error {
	if r.SchemaVersion == "" {
		return fmt.Errorf("%w: routing.schemaVersion is required", ErrEvidenceInvalid)
	}
	if !r.Enabled && strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: routing.reason is required when routing did not run", ErrEvidenceInvalid)
	}
	if r.Sections < 0 || r.Dropped < 0 || r.Truncated < 0 {
		return fmt.Errorf("%w: routing section counts must not be negative", ErrEvidenceInvalid)
	}
	return nil
}

// routingContextKey is the key the routing block travels under between the
// dispatch wrappers.
type routingContextKey struct{}

// WithRouting carries a routing block down a dispatch call so the recorder
// wrapper inside it can attach the block to that dispatch's evidence record.
//
// The two wrappers are deliberately independent — contextrouter/wfrouter
// decides what to send, observe/projectmemory/wfdispatch records what was
// sent, and either can be installed without the other. The context is what
// lets the outer one hand a fact to the inner one without either taking a
// dependency on the other's installation.
func WithRouting(ctx stdctx.Context, routing RoutingMetrics) stdctx.Context {
	if ctx == nil {
		return nil
	}
	return stdctx.WithValue(ctx, routingContextKey{}, routing.normalized())
}

// RoutingFromContext returns the routing block a dispatch carries, if any.
func RoutingFromContext(ctx stdctx.Context) (RoutingMetrics, bool) {
	if ctx == nil {
		return RoutingMetrics{}, false
	}
	routing, ok := ctx.Value(routingContextKey{}).(RoutingMetrics)
	return routing, ok
}
