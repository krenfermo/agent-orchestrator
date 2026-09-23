package skillreport

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The planted value is deliberately NOT shaped like any known token: it is the
// case only the harvested-literal net can catch.
const planted = "Tr0ub4dor-prod-9931"

func TestRedact_ShapesAreReplacedWherever_TheyAppear(t *testing.T) {
	r := NewRedactor(nil)
	for name, in := range map[string]string{
		"aws key":      "found AKIAIOSFODNN7EXAMPLE in config",
		"github token": "token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB leaked",
		"private key":  "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
		"jwt":          "cookie eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N",
		"url password": "postgres://app:s3cretpw@db:5432/app",
		"assignment":   `password = "correct-horse-battery"`,
		"anthropic":    "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123",
	} {
		t.Run(name, func(t *testing.T) {
			out, n := r.String(in)
			if n == 0 || !strings.Contains(out, Marker) {
				t.Fatalf("not redacted: %q -> %q", in, out)
			}
		})
	}
}

func TestRedact_HarvestedLiteralAndItsDisguises(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.go"),
		[]byte(`package x
var dbPassword = "`+planted+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("API_TOKEN=plainvalue77\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lits, err := HarvestLiterals(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRedactor(lits)
	sum := sha256.Sum256([]byte(planted))
	for name, in := range map[string]string{
		"verbatim":  "the password is " + planted + " in config.go",
		"prefix":    "starts with " + planted[:9] + "...",
		"sha256":    "digest " + hex.EncodeToString(sum[:]),
		"base64":    "encoded " + base64.StdEncoding.EncodeToString([]byte(planted)),
		"env value": "API_TOKEN is plainvalue77",
	} {
		t.Run(name, func(t *testing.T) {
			out, n := r.String(in)
			if n == 0 || strings.Contains(out, planted[:minPrefix]) || strings.Contains(out, "plainvalue77") {
				t.Fatalf("not redacted: %q -> %q", in, out)
			}
		})
	}
}

// Redaction must reach every string in the document, not only the fields a
// secret is expected in.
func TestRedact_WalksTheWholeDocument(t *testing.T) {
	doc := strings.Replace(validReport, `Add tenant_id to the WHERE clause.`, `Rotate `+planted, 1)
	doc = strings.Replace(doc, `"GET /orders/2 as tenant 1"`, `"login with `+planted+`"`, 1)
	v, err := Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out, n := NewRedactor([]string{planted}).Redact(v)
	if n != 2 {
		t.Fatalf("redactions = %d, want 2", n)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), planted) {
		t.Fatalf("planted value survived: %s", b)
	}
	if err := canonical(t).ValidateValue(out); err != nil {
		t.Fatalf("redacted report no longer validates: %v", err)
	}
}

// Ordinary prose must survive: a redactor that rewrote every word after
// "password" would make the report useless.
func TestRedact_LeavesProseAlone(t *testing.T) {
	r := NewRedactor(nil)
	for _, in := range []string{
		"Password hashing uses bcrypt with cost 12.",
		"The token is validated in middleware/auth.go before the handler runs.",
		"secret rotation is not implemented",
	} {
		if out, n := r.String(in); n != 0 {
			t.Fatalf("prose rewritten: %q -> %q", in, out)
		}
	}
}
