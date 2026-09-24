package skillreport

import (
	"embed"
	"fmt"
)

//go:embed schemas/security-audit.v1.json
var auditFS embed.FS

// AuditSchemaVersion is the consolidated audit report's schemaVersion.
const AuditSchemaVersion = "ao.security-audit/v1"

// AuditSchema is the JSON Schema of the consolidated security audit report
// (2E). It is AO's own contract -- the audit parent writes it -- and it is
// validated with the same strict validator as an agent's report, before and
// after redaction, like every other report AO stores from a composed source.
func AuditSchema() *Schema {
	raw, err := auditFS.ReadFile("schemas/security-audit.v1.json")
	if err != nil {
		panic(fmt.Sprintf("skillreport: embedded audit schema missing: %v", err))
	}
	s, err := ParseSchema(raw)
	if err != nil {
		panic(fmt.Sprintf("skillreport: embedded audit schema is outside the supported subset: %v", err))
	}
	return s
}
