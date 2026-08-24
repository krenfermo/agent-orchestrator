package projectmemory

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestMetricLabelingRule(t *testing.T) {
	value := int64(7)
	negative := int64(-1)
	zero := int64(0)
	cases := []struct {
		name    string
		metric  Metric
		wantErr bool
	}{
		{"measured with value and method", Measured(12, "counted bytes"), false},
		{"estimated with value and method", Estimated(3, EstimateMethod), false},
		{"unavailable with reason", Unavailable("provider reported nothing"), false},
		{"measured zero is a real observation", Measured(0, "counted bytes"), false},
		{"measured without a value", Metric{Basis: BasisMeasured, Method: "counted bytes"}, true},
		{"estimated without a value", Metric{Basis: BasisEstimated, Method: EstimateMethod}, true},
		{"measured without a method", Metric{Value: &value, Basis: BasisMeasured}, true},
		{"estimated without a method", Metric{Value: &value, Basis: BasisEstimated}, true},
		{"unavailable carrying a value", Metric{Value: &zero, Basis: BasisUnavailable, Method: "why"}, true},
		{"unavailable without a reason", Metric{Basis: BasisUnavailable}, true},
		{"negative count", Metric{Value: &negative, Basis: BasisMeasured, Method: "counted bytes"}, true},
		{"missing basis", Metric{Value: &value, Method: "counted bytes"}, true},
		{"unknown basis", Metric{Value: &value, Basis: Basis("guessed"), Method: "counted bytes"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.metric.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected %+v to be rejected", tc.metric)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected %+v to be accepted, got %v", tc.metric, err)
			}
			if tc.wantErr && !errors.Is(err, ErrMetricInvalid) {
				t.Fatalf("expected ErrMetricInvalid, got %v", err)
			}
		})
	}
}

// An unavailable metric must stay null. Substituting zero is the exact failure
// this package exists to prevent: a reader cannot tell a real zero from a
// missing measurement once it has happened.
func TestUnavailableMetricCarriesNoValue(t *testing.T) {
	m := Unavailable("no telemetry")
	if m.Value != nil {
		t.Fatalf("unavailable metric carried value %d", *m.Value)
	}
	if m.Basis != BasisUnavailable {
		t.Fatalf("basis = %q", m.Basis)
	}
	if m.Method != "no telemetry" {
		t.Fatalf("reason = %q", m.Method)
	}
}

func TestMeasuredOrUnavailable(t *testing.T) {
	if got := MeasuredOrUnavailable(nil, "telemetry", "provider sent none"); got.Basis != BasisUnavailable || got.Value != nil {
		t.Fatalf("nil pointer must become unavailable, got %+v", got)
	}
	zero := int64(0)
	got := MeasuredOrUnavailable(&zero, "telemetry", "provider sent none")
	if got.Basis != BasisMeasured || got.Value == nil || *got.Value != 0 {
		t.Fatalf("a reported zero must stay a measured zero, got %+v", got)
	}
}

func TestBasisMapsOntoExistingCertaintyVocabulary(t *testing.T) {
	cases := map[Basis]domain.MetricCertainty{
		BasisMeasured:     domain.MetricActual,
		BasisEstimated:    domain.MetricInferred,
		BasisUnavailable:  domain.MetricUnknown,
		Basis("nonsense"): domain.MetricUnknown,
	}
	for basis, want := range cases {
		if got := basis.Certainty(); got != want {
			t.Fatalf("%q.Certainty() = %q, want %q", basis, got, want)
		}
	}
}

func TestNormalizedFillsAbsenceNeverANumber(t *testing.T) {
	got := Metric{}.normalized()
	if got.Basis != BasisUnavailable {
		t.Fatalf("basis = %q, want unavailable", got.Basis)
	}
	if got.Value != nil {
		t.Fatalf("normalizing an empty metric invented the value %d", *got.Value)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("normalized metric must be valid: %v", err)
	}
	measured := Measured(4, "counted")
	if got := measured.normalized(); got != measured {
		t.Fatalf("normalizing a populated metric changed it: %+v", got)
	}
}

func TestEstimateTokensFromBytes(t *testing.T) {
	cases := map[int64]int64{0: 0, -5: 0, 1: 1, 4: 1, 5: 2, 4096: 1024}
	for in, want := range cases {
		if got := EstimateTokensFromBytes(in); got != want {
			t.Fatalf("EstimateTokensFromBytes(%d) = %d, want %d", in, got, want)
		}
	}
	if m := EstimatedTokensFor(100); m.Basis != BasisEstimated || m.Method != EstimateMethod {
		t.Fatalf("a byte-derived token count must be labeled estimated and name its heuristic, got %+v", m)
	}
}
