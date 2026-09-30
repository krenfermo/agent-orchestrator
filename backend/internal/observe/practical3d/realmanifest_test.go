package practical3d

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBuildRealMiniManifestValidates(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env, err := LiveEnvironmentObserver{Expected: EnvironmentInputs{RuntimeVersions: []VersionInput{{Component: "go"}}, ProviderClientCLIVersions: []VersionInput{}, TaskToolVersions: []VersionInput{{Component: "git"}}, RunnerInstrumentVersions: []VersionInput{{Component: "ao3dpractical"}}, EffectiveEnvironmentConfigAllowlist: []ConfigInput{}, AdditionalLocalConfiguration: []ConfigInput{}}, AOBinaryPath: self}.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.AOCommit = strings.Repeat("a", 40)
	specs := map[string][]byte{}
	hidden := map[string][]byte{}
	for _, task := range taskOrder {
		b, _ := json.Marshal(TaskSpec{Schema: TaskSpecSchema, TaskID: task, Objective: "do " + task, ReviewDepth: "none", WriteIntent: "mutating", Verification: json.RawMessage(`{"commands":[]}`), OracleTask: task})
		specs[task] = b
		hidden[task] = []byte(`{"task":"` + task + `"}`)
	}
	file := []byte("package orders\n\nfunc Quote() { code.Amount(total) }\n")
	m, blobs, err := BuildRealMiniManifest(RealMiniInputs{AOCommit: env.AOCommit, FixtureCommit: strings.Repeat("b", 40), AccountRefSHA256: sha256Hex([]byte("org")), ClaudeVersion: "2.1.285",
		PrimaryModel: "claude-sonnet-5-5", HelperModel: "claude-haiku-4-5", Env: env, TaskSpecs: specs, Attachment: []byte("MEMORY FRESHNESS: CURRENT\n\n## pack\n"), AttachmentRef: "attachment-A.bin",
		OracleScript: []byte("#!/bin/bash\n"), HiddenManifests: hidden, VerifyCommand: "/bin/sh -c go test ./...", FixtureSubtree: sha256Hex([]byte("tree")),
		ReviewTarget: []byte("diff"), ReviewFile: file, ReviewFilePath: "internal/orders/pricing.go", ReviewCausalLine: 3, IndexedCommit: strings.Repeat("b", 40), PackDigest: sha256Hex([]byte("pack"))})
	if err != nil {
		t.Fatal(err)
	}
	if !IsTechnicalManifest(m) || len(blobs) == 0 {
		t.Fatal("real mini manifest must be technical")
	}
	raw, _ := CanonicalManifest(m)
	if _, err := DecodeManifest(raw); err != nil {
		t.Fatal(err)
	}
}
