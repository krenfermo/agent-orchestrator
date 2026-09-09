package skillrunner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A manifest may name a tool from AO's closed vocabulary. It may not name an
// image, a binary, or a single argument. This is the difference between "the
// package declares what it wants" and "the package decides what executes as
// us", and it is why arbitrary_process_execution stays a separate control.
func TestApprovedTools_IsAClosedVocabulary(t *testing.T) {
	tools := ApprovedTools()
	if len(tools) != 1 || tools[0] != ToolStaticScan {
		t.Fatalf("approved tools = %v; adding one must be a deliberate code change", tools)
	}
	for _, tool := range tools {
		contract, ok := approvedTools[tool]
		if !ok || contract.Argv == nil || contract.BaseImage == "" {
			t.Fatalf("%s has an incomplete contract: %+v", tool, contract)
		}
		// The digest is resolved from the host at run time, never written here
		// as a constant somebody could quietly change to a different image.
		if contract.Digest != "" {
			t.Fatalf("%s ships a hardcoded digest: %q", tool, contract.Digest)
		}
		if strings.Contains(contract.BaseImage, "latest") {
			t.Fatalf("%s uses a moving tag: %q", tool, contract.BaseImage)
		}
	}
}

func TestResolveContract_RefusesAnUnapprovedTool(t *testing.T) {
	r := &Runner{runtime: Runtime{Binary: "docker"}, runner: fakeCLI{}}
	_, err := r.ResolveContract(context.Background(), Tool("nmap"))
	if !errors.Is(err, ErrToolNotApproved) {
		t.Fatalf("err = %v, want ErrToolNotApproved", err)
	}
}

func TestResolveContract_RefusesWhenAOCannotProveWhatWouldRun(t *testing.T) {
	cases := []struct {
		name    string
		cli     fakeCLI
		wantSub string
	}{
		{
			"base image absent, and AO pulls nothing",
			fakeCLI{err: errors.New("No such image")},
			"is not present on this host",
		},
		{
			"runtime answered with something that is not a digest",
			fakeCLI{out: []byte("alpine:3.19\n")},
			"is not a content digest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{runtime: Runtime{Binary: "docker"}, runner: tc.cli}
			_, err := r.ResolveContract(context.Background(), ToolStaticScan)
			if !errors.Is(err, ErrRuntimeUnavailable) {
				t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

func TestResolveContract_PinsByDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	r := &Runner{runtime: Runtime{Binary: "docker"}, runner: fakeCLI{out: []byte(digest + "\n")}}
	contract, err := r.ResolveContract(context.Background(), ToolStaticScan)
	if err != nil {
		t.Fatalf("ResolveContract: %v", err)
	}
	if contract.PinnedRef() != "alpine@"+digest {
		t.Fatalf("pinned ref = %q", contract.PinnedRef())
	}
	if strings.Contains(contract.PinnedRef(), ":3.19") {
		t.Fatalf("the pinned reference still carries a tag: %q", contract.PinnedRef())
	}
}

func TestToolParams_BoundsWhatReachesACommandLine(t *testing.T) {
	if err := DefaultToolParams().Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	for _, bad := range []ToolParams{
		{MaxFiles: 0, MaxFileBytes: 1 << 20},
		{MaxFiles: 200000, MaxFileBytes: 1 << 20},
		{MaxFiles: 10, MaxFileBytes: 10},
		{MaxFiles: 10, MaxFileBytes: 1 << 30},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
}

// The rules reach the container inside a QUOTED heredoc (<<'EOF'), where
// nothing is interpreted — a single quote is just a character, which is why
// the TLS and hash rules can contain one. The real hazards are a line that
// equals a heredoc terminator, a newline that would split one rule into two,
// and the separator byte the line protocol uses. This fails the build for a
// future rule that carries any of them.
func TestStaticScanRules_CannotBreakTheirOwnEncoding(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range staticScanRules {
		if seen[rule.ID] {
			t.Fatalf("duplicate rule id %s", rule.ID)
		}
		seen[rule.ID] = true
		for field, value := range map[string]string{
			"pattern": rule.Pattern, "title": rule.Title, "id": rule.ID,
			"severity": rule.Severity, "category": rule.Category,
		} {
			if strings.ContainsAny(value, "\n\r\x1f") {
				t.Fatalf("rule %s %s contains a newline or the separator byte: %q",
					rule.ID, field, value)
			}
			for _, terminator := range []string{"AO_RULES_EOF", "AO_PATTERNS_EOF"} {
				if strings.Contains(value, terminator) {
					t.Fatalf("rule %s %s contains a heredoc terminator: %q", rule.ID, field, value)
				}
			}
		}
		if rule.Recommendation == "" {
			t.Fatalf("rule %s has no recommendation", rule.ID)
		}
	}
}

// Only the two validated integers vary. Nothing a manifest can express reaches
// the command line.
func TestStaticScanArgv_TakesNothingFromAManifest(t *testing.T) {
	argv := staticScanArgv(ToolParams{MaxFiles: 1234, MaxFileBytes: 4321})
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv shape = %v", argv)
	}
	script := argv[2]
	if !strings.Contains(script, "MAX_FILES=1234") || !strings.Contains(script, "MAX_BYTES=4321") {
		t.Fatal("the validated params did not reach the script")
	}
	// The evidence probe must be in the same command as the scan: a report
	// from a run that cannot show it was confined is not evidence.
	for _, want := range []string{"ao_uid=", "ao_input_files=", "ao_network_reachable="} {
		if !strings.Contains(script, want) {
			t.Fatalf("the script does not emit %s", want)
		}
	}
	// -H is load-bearing for a single-file scope; without it grep omits the
	// filename and every finding parses as pathless.
	if !strings.Contains(script, "grep -HInE") {
		t.Fatal("grep is missing -H, so a one-file scope would silently find nothing")
	}
}
