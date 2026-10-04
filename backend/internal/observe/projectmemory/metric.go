// Package projectmemory records what one AO agent dispatch actually had
// available to it and what it actually consumed: which files were inspected,
// how much source the task's scope made reachable, how much context AO really
// sent, what the provider reported back, and how the run's review/verify
// outcomes turned out.
//
// It is the Phase 0 baseline harness for AO's project-memory work: it measures
// the CURRENT pipeline rather than changing it. Every dispatch surface is
// instrumented by wrapping, never by editing the dispatcher (see the
// wfdispatch subpackage), and every number carries the basis it was obtained
// on, so a later phase can compare against this baseline without having to
// guess which figures were real.
//
// The one rule this package exists to enforce is that a number AO could not
// measure is never presented as if it had been measured: see Metric and
// docs/project-memory-baseline.md.
package projectmemory

import (
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Basis says how a recorded number was obtained. It is written into every
// evidence record next to the number itself, because "12000 tokens" means
// something entirely different when a provider reported it than when AO
// divided a byte count by four.
type Basis string

// Basis values.
const (
	// BasisMeasured is a number AO observed directly: bytes it read, a
	// duration it timed, a count of calls it made, or a figure a provider's
	// own telemetry reported.
	BasisMeasured Basis = "measured"
	// BasisEstimated is a number AO derived from something it measured, via a
	// heuristic that Metric.Method must name. It is a defensible approximation,
	// not an observation.
	BasisEstimated Basis = "estimated"
	// BasisUnavailable is the honest answer when a metric could not be
	// obtained at all. Metric.Value is nil in this case and Metric.Method
	// carries the reason. Nothing in this package ever substitutes zero.
	BasisUnavailable Basis = "unavailable"
)

// Certainty maps a Basis onto the metric-certainty vocabulary the rest of the
// codebase already uses (domain.MetricCertainty), so baseline evidence and the
// usage read models describe the same idea with one set of words instead of
// two.
func (b Basis) Certainty() domain.MetricCertainty {
	switch b {
	case BasisMeasured:
		return domain.MetricActual
	case BasisEstimated:
		return domain.MetricInferred
	case BasisUnavailable:
		return domain.MetricUnknown
	default:
		return domain.MetricUnknown
	}
}

// Metric is one non-negative count plus how it was obtained. Value is a
// pointer so that "not known" is representable as JSON null and can never be
// confused with a measured zero — a distinction the whole baseline depends on,
// since a run that genuinely sent no context and a run whose context AO could
// not observe are different findings.
//
// Method is required in all three cases and says either how the number was
// observed, which heuristic produced it, or why it is missing.
type Metric struct {
	Value  *int64 `json:"value"`
	Basis  Basis  `json:"basis"`
	Method string `json:"method"`
}

// ErrMetricInvalid is the sentinel every Metric labeling-rule violation wraps,
// so callers can test for a schema violation without matching on message text.
var ErrMetricInvalid = errors.New("invalid metric")

// Measured builds a metric for a number AO observed directly. method names
// what was observed (e.g. "utf8 byte length of the prompt AO sent").
func Measured(value int64, method string) Metric {
	return Metric{Value: &value, Basis: BasisMeasured, Method: method}
}

// Estimated builds a metric for a number AO derived from a measurement.
// method must name the heuristic, because an estimate whose derivation is not
// stated cannot be re-derived or improved on later.
func Estimated(value int64, method string) Metric {
	return Metric{Value: &value, Basis: BasisEstimated, Method: method}
}

// Unavailable builds a metric for something AO could not obtain. reason says
// why, and the value stays nil.
func Unavailable(reason string) Metric {
	return Metric{Basis: BasisUnavailable, Method: reason}
}

// MeasuredOrUnavailable is the bridge from the nullable counters provider
// telemetry hands AO (domain.UsageMetricTotals and friends): a populated
// pointer is a measured fact, a nil pointer is unavailable — never zero.
func MeasuredOrUnavailable(value *int64, method, reason string) Metric {
	if value == nil {
		return Unavailable(reason)
	}
	return Measured(*value, method)
}

// Validate enforces the labeling rule this package exists for:
//
//   - an unavailable metric carries no value and states a reason;
//   - a measured or estimated metric carries a value and states how it was
//     obtained;
//   - no count is negative;
//   - no metric is silently unlabeled.
//
// Validate is called on every metric in an evidence record before that record
// is written, so a violation fails the write instead of producing a plausible
// but dishonest file.
func (m Metric) Validate() error {
	switch m.Basis {
	case BasisUnavailable:
		if m.Value != nil {
			return fmt.Errorf("%w: unavailable metric must not carry a value", ErrMetricInvalid)
		}
		if m.Method == "" {
			return fmt.Errorf("%w: unavailable metric must state a reason", ErrMetricInvalid)
		}
		return nil
	case BasisMeasured, BasisEstimated:
		if m.Value == nil {
			return fmt.Errorf("%w: %s metric must carry a value", ErrMetricInvalid, m.Basis)
		}
		if *m.Value < 0 {
			return fmt.Errorf("%w: %s metric must not be negative (got %d)", ErrMetricInvalid, m.Basis, *m.Value)
		}
		if m.Method == "" {
			return fmt.Errorf("%w: %s metric must state how it was obtained", ErrMetricInvalid, m.Basis)
		}
		return nil
	case "":
		return fmt.Errorf("%w: metric must state its basis", ErrMetricInvalid)
	default:
		return fmt.Errorf("%w: unknown basis %q", ErrMetricInvalid, m.Basis)
	}
}

// normalized turns a never-populated metric into an explicit "not recorded"
// rather than letting a zero value reach the file as an unlabeled blank. It is
// the only place a metric is filled in on the caller's behalf, and it fills in
// an absence, never a number.
func (m Metric) normalized() Metric {
	if m.Basis == "" && m.Value == nil && m.Method == "" {
		return Unavailable("not recorded by the baseline harness")
	}
	return m
}
