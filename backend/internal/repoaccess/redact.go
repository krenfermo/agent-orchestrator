package repoaccess

import (
	"regexp"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
)

// redact.go — the redaction boundary for repository-derived text.
//
// The secret boundary (secret.go) keeps files that ARE secrets from ever being
// opened. It cannot catch a secret INSIDE an ordinary file: a password in a
// docker-compose environment block, a token pasted into a README, a key in a
// doc comment. Everything AO derives from repository text and then persists
// or hands to an agent -- memory items, graph symbol docs/signatures/
// summaries, rendered context packs, API output -- passes through Redact.
//
// It REUSES the Skills report redactor's credential shapes (cloud keys, forge
// tokens, private-key blocks, JWTs, credentials in URLs, credential-named
// assignments) rather than keeping a second list that would drift, and adds
// the one shape repository config needs that a report rarely does: the
// unquoted YAML form `POSTGRES_PASSWORD: hunter2hunter2`.

// Marker replaces a redacted value.
const Marker = skillreport.Marker

// yamlCredential is `name: value` (YAML) or `name=value` (.properties, .ini,
// lowercase env) with a credential-shaped name and an unquoted value. Group 1 keeps the key (so the fact still says WHAT is
// configured); group 2 is the value.
var yamlCredential = regexp.MustCompile(
	`(?im)^(\s*-?\s*["']?[A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[-_]?key|apikey|access[-_]?key|private[-_]?key|client[-_]?secret)[A-Za-z0-9_.-]*["']?\s*(?::|=)\s*)([^\s#"'{}\[\]][^\s#]{5,})`)

var shapeRedactor = skillreport.NewRedactor(nil)

// Redact returns s with every recognised credential replaced by Marker, and
// how many were replaced. It is deterministic, so redacting the same input
// twice gives the same output (a reconfirmed fact keeps its content hash).
func Redact(s string) (string, int) {
	if s == "" {
		return s, 0
	}
	out, n := shapeRedactor.String(s)
	count := 0
	out = yamlCredential.ReplaceAllStringFunc(out, func(m string) string {
		sub := yamlCredential.FindStringSubmatch(m)
		if len(sub) < 3 || sub[2] == Marker {
			return m
		}
		count++
		return sub[1] + Marker
	})
	return out, n + count
}

// RedactString is Redact without the count.
func RedactString(s string) string {
	out, _ := Redact(s)
	return out
}
