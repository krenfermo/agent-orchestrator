package skillreport

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// validReport is a complete findings.v1 document.
const validReport = `{
  "schemaVersion": "security-audit/findings/v1",
  "run": {"projectId": "p1", "mode": "authz-review", "startedAt": "2026-09-23T10:00:00Z",
          "endedAt": "2026-09-23T10:05:00Z", "skillVersion": "0.2.0"},
  "coverage": {"examined": ["api/handler.go"], "skipped": [{"path": ".env", "reason": "denied"}]},
  "findings": [{
    "id": "AUTHZ-1", "title": "Order lookup skips the tenant predicate", "severity": "high",
    "confidence": "probable", "category": "idor", "cwe": "CWE-639",
    "evidence": {"summary": "GetOrder reads by id before checking tenant",
                 "locations": [{"path": "api/handler.go", "line": 12}]},
    "reproduction": {"reproducible": false, "steps": ["GET /orders/2 as tenant 1"]},
    "falsePositiveAssessment": {"consideredAlternatives": ["middleware"], "whyNotAFalsePositive": "none applies"},
    "recommendation": "Add tenant_id to the WHERE clause."
  }],
  "notes": []
}`

func canonical(t *testing.T) *Schema {
	t.Helper()
	s, err := ParseSchema(skillcatalog.CanonicalFindingsSchema())
	if err != nil {
		t.Fatalf("the canonical findings schema must be inside the supported subset: %v", err)
	}
	return s
}

func TestValidate_AcceptsACompleteReport(t *testing.T) {
	if err := canonical(t).Validate([]byte(validReport)); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
}

func TestValidate_RejectsEveryDeviation(t *testing.T) {
	s := canonical(t)
	cases := map[string]string{
		"not json":            `{"schemaVersion": `,
		"trailing data":       validReport + ` {}`,
		"free text":           `The audit found no issues.`,
		"wrong schemaVersion": strings.Replace(validReport, `security-audit/findings/v1`, `v2`, 1),
		"unknown property":    strings.Replace(validReport, `"notes": []`, `"notes": [], "grantCapability": "net.egress"`, 1),
		"missing coverage":    strings.Replace(validReport, `"coverage": {"examined": ["api/handler.go"], "skipped": [{"path": ".env", "reason": "denied"}]},`, ``, 1),
		"severity enum":       strings.Replace(validReport, `"severity": "high"`, `"severity": "catastrophic"`, 1),
		"id pattern":          strings.Replace(validReport, `"AUTHZ-1"`, `"authz one"`, 1),
		"no locations":        strings.Replace(validReport, `"locations": [{"path": "api/handler.go", "line": 12}]`, `"locations": []`, 1),
		"line not integer":    strings.Replace(validReport, `"line": 12`, `"line": 12.5`, 1),
		"line below minimum":  strings.Replace(validReport, `"line": 12`, `"line": 0`, 1),
		"bad date-time":       strings.Replace(validReport, `"2026-09-23T10:00:00Z"`, `"yesterday"`, 1),
		"title too long":      strings.Replace(validReport, `Order lookup skips the tenant predicate`, strings.Repeat("x", 121), 1),
		"wrong type":          strings.Replace(validReport, `"reproducible": false`, `"reproducible": "no"`, 1),
		// encoding/json keeps the LAST duplicate; a validator on top of it
		// would check one value while a later reader saw another.
		"duplicate key": strings.Replace(validReport, `"severity": "high",`, `"severity": "high", "severity": "info",`, 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Validate([]byte(doc))
			if !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("err = %v, want ErrInvalidReport", err)
			}
		})
	}
}

func TestValidate_RefusesOversizeBeforeParsing(t *testing.T) {
	big := make([]byte, MaxReportBytes+1)
	if err := canonical(t).Validate(big); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("err = %v", err)
	}
}

// A validator that skipped a keyword it did not implement would accept exactly
// what that keyword exists to reject.
func TestParseSchema_RefusesUnsupportedKeywords(t *testing.T) {
	for name, schema := range map[string]string{
		"oneOf":                     `{"type":"object","oneOf":[{"type":"object"}]}`,
		"$ref":                      `{"$ref":"#/defs/x"}`,
		"additionalProperties true": `{"type":"object","additionalProperties":true}`,
		"unknown format":            `{"type":"string","format":"email"}`,
		"tuple items":               `{"type":"array","items":[{"type":"string"}]}`,
		"nested unsupported":        `{"type":"object","properties":{"a":{"type":"string","contentEncoding":"base64"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSchema([]byte(schema)); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
			}
		})
	}
}
