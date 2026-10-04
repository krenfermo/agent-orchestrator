package skillrunner

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// staticscan_rules_test.go — AOSS-006, the credential-shaped-literal rule.
//
// The defect: the rules run under `grep -HInE` (capital I is "skip binaries";
// there is no -i), so the rule was case-SENSITIVE and could not see `apiKey`.
// Go, JS and TS spell it that way nearly always, so a critical-severity secret
// rule returned a clean report on a tree that had the secret in it.
//
// The fix widens ONE rule with explicit character classes rather than adding -i
// to the shared grep. TestStaticScanRules_NoGlobalIgnoreCase records why: -i
// would apply to all eight rules, and AOSS-003 matching `exec(` becomes
// `cmd.Exec(` in every Go codebase.
//
// These run the pattern through Go's regexp for a fast, deterministic table.
// The engine that actually runs it is busybox grep inside alpine, which is
// covered end-to-end by the live container test.

func ruleByID(t *testing.T, id string) scanRule {
	t.Helper()
	for _, r := range staticScanRules {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("rule %s is gone", id)
	return scanRule{}
}

func TestAOSS006_DetectsTheCommonSpellingsOfACredentialAssignment(t *testing.T) {
	rule := ruleByID(t, "AOSS-006")
	if rule.Severity != "critical" {
		t.Fatalf("severity = %q; widening the rule must not soften it", rule.Severity)
	}
	re, err := regexp.Compile(rule.Pattern)
	if err != nil {
		t.Fatalf("pattern does not compile: %v", err)
	}

	positive := map[string][]string{
		"go": {
			// The pilot-2 fixture, unchanged. This is the case that failed.
			`const apiKey = "AKIAIOSFODNN7EXAMPLE"`,
			`apiKey := "AKIAIOSFODNN7EXAMPLE"`,
			`apiToken := "ghp_abcdefghijklmnop"`,
			`privateKey = "-----BEGIN RSA PRIVATE KEY-----"`,
			`secretKey := "s3cr3tv4lue_long_enough"`,
			`accessKey = "AKIAIOSFODNN7EXAMPLE"`,
			`password := "hunter2hunter2hunter2"`,
		},
		"js/ts": {
			`const apiKey = "sk-abcdefghijklmnop"`,
			`const api_key = "AKIAIOSFODNN7EXAMPLE"`,
			`const apiToken = 'ghp_abcdefghijklmnop'`,
			`{ apiKey: "AKIAIOSFODNN7EXAMPLE" }`,
			`private_key: "-----BEGIN RSA PRIVATE KEY-----"`,
		},
		"config": {
			`api-key: "AKIAIOSFODNN7EXAMPLE"`,
			`api_token: "ghp_abcdefghijklmnop"`,
			`API_KEY = "AKIAIOSFODNN7EXAMPLE"`,
			`ACCESS_KEY: "AKIAIOSFODNN7EXAMPLE"`,
			`Password = "hunter2hunter2hunter2"`,
		},
	}
	for lang, lines := range positive {
		for _, line := range lines {
			if !re.MatchString(line) {
				t.Errorf("[%s] MISSED a credential assignment: %s", lang, line)
			}
		}
	}
}

// The rule must not fire on the WORD. A mention in a comment, an identifier, an
// indirection through the environment and an empty or short value are all
// things a real codebase is full of, and a rule that flagged them would be
// turned off within a week — which is a worse outcome than the false negative
// it was fixed for.
func TestAOSS006_DoesNotFireWithoutASecretShapedValue(t *testing.T) {
	rule := ruleByID(t, "AOSS-006")
	re := regexp.MustCompile(rule.Pattern)

	negative := map[string][]string{
		"go": {
			`// TODO: read the apiKey from the environment before shipping`,
			`apiKey = os.Getenv("API_KEY")`,
			`apiKey := cfg.APIKey`,
			`apiKey = ""`,
			`apiKey = "short"`,
			`func setAPIKey(v string) error { return nil }`,
			`const apiKeyHeader = 8`,
			`tokenBucket.Refill(rateLimitPerSecond)`,
		},
		"js/ts": {
			`const apiKey = process.env.API_KEY`,
			`const apiKey = apiKeyFromVault()`,
			`type ApiKey = string`,
			`// apiKey: see the deployment runbook`,
		},
		"config": {
			`# api-key: set this in the vault, not here`,
			`api-key:`,
			`api_key: ${API_KEY}`,
		},
	}
	for lang, lines := range negative {
		for _, line := range lines {
			if re.MatchString(line) {
				t.Errorf("[%s] FALSE POSITIVE on: %s", lang, line)
			}
		}
	}
}

// A known miss, recorded rather than hidden.
//
// The rule requires a QUOTED literal, which is what keeps it off `token:
// ${VAULT}` and every placeholder in every config file. The cost is that an
// unquoted YAML value is not seen. Widening to unquoted values is a false-
// positive trade nobody has made yet, so it is written down here: if this test
// ever starts failing, the trade was made and this note should go with it.
func TestAOSS006_KnownMiss_UnquotedConfigValues(t *testing.T) {
	re := regexp.MustCompile(ruleByID(t, "AOSS-006").Pattern)
	unquoted := `api_key: AKIAIOSFODNN7EXAMPLE`
	if re.MatchString(unquoted) {
		t.Fatalf("the quoted-literal requirement was dropped: %s now matches. "+
			"That is a deliberate trade against false positives on placeholders "+
			"like `api_key: ${VAULT}`; make it knowingly, and update this test.", unquoted)
	}
}

// Why the fix is per-rule and not `grep -i`.
//
// This is the evaluation the widening rests on, kept executable so it is not
// just a claim in a comment: under -i, AOSS-003 matches ordinary Go.
func TestStaticScanRules_NoGlobalIgnoreCase(t *testing.T) {
	script := staticScanArgv(DefaultToolParams())[2]
	if strings.Contains(script, "grep -HIniE") || strings.Contains(script, "grep -i") {
		t.Fatal("the scan added a global -i; that widens all eight rules at once")
	}

	// The concrete damage, demonstrated rather than asserted. AOSS-003 already
	// guards against a preceding dot, so `cmd.Exec(` is safe either way; what
	// -i breaks is the declaration and the plain call, which every Go file has.
	damage := []struct{ rule, line string }{
		{"AOSS-003", `func Exec(ctx context.Context) error {`},
		{"AOSS-003", `if err := Exec(cmd); err != nil {`},
		{"AOSS-008", `debug: 1`},
	}
	for _, d := range damage {
		pattern := ruleByID(t, d.rule).Pattern
		if regexp.MustCompile(pattern).MatchString(d.line) {
			t.Fatalf("%s already matches %q as shipped; the example is stale", d.rule, d.line)
		}
		if !regexp.MustCompile(`(?i)` + pattern).MatchString(d.line) {
			t.Fatalf("expected -i to make %s match %q; if it no longer does, "+
				"re-evaluate whether a global -i is now safe", d.rule, d.line)
		}
	}
}

// The pattern has to be an ERE that BUSYBOX grep accepts, not just one Go's
// regexp compiles. When a POSIX grep is on the host, use it; the live container
// test covers busybox itself.
func TestAOSS006_IsAPortableERE(t *testing.T) {
	grep, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("no grep on this host; the live container test covers the real engine")
	}
	rule := ruleByID(t, "AOSS-006")
	cmd := exec.Command(grep, "-HInE", "--", rule.Pattern) //nolint:gosec // pattern is a package constant.
	cmd.Stdin = strings.NewReader("const apiKey = \"AKIAIOSFODNN7EXAMPLE\"\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("grep -E rejected the pattern or found nothing: %v", err)
	}
	if !strings.Contains(string(out), "apiKey") {
		t.Fatalf("grep -E did not match the fixture line: %q", out)
	}
}
