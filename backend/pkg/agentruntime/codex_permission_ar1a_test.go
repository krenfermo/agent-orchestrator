package agentruntime

import (
	"slices"
	"testing"
)

// AR-1a / D-SEC-4: Codex never starts with the sandbox and approvals bypassed
// unless the policy says so explicitly.
func TestCodexBypassRequiresTheExplicitPolicy(t *testing.T) {
	const bypass = "--dangerously-bypass-approvals-and-sandbox"
	for _, policy := range []PermissionPolicy{"", PermissionDefault, "BYPASS-PERMISSIONS", "bypass", "yolo", "danger-full-access", "trusted", " bypass-permissions"} {
		args := CodexPermissionArgs(policy)
		if slices.Contains(args, bypass) {
			t.Fatalf("policy %q produced the bypass: %v", policy, args)
		}
		if !slices.Equal(args, CodexDefaultSandboxArgs) {
			t.Fatalf("policy %q = %v, want the sandboxed default %v", policy, args, CodexDefaultSandboxArgs)
		}
	}
	if args := CodexPermissionArgs(PermissionBypassPermissions); !slices.Equal(args, []string{bypass}) {
		t.Fatalf("explicit bypass-permissions = %v, want exactly the bypass flag", args)
	}
}

// The sandboxed default is a copy: a caller appending to it cannot change the
// default for every later launch.
func TestCodexDefaultSandboxArgsAreNotAliased(t *testing.T) {
	args := CodexPermissionArgs(PermissionDefault)
	args[1] = "danger-full-access"
	if CodexDefaultSandboxArgs[1] != "workspace-write" {
		t.Fatalf("mutating a returned slice changed the default: %v", CodexDefaultSandboxArgs)
	}
}

// The full launch and restore commands carry the same default posture.
func TestCodexLaunchAndRestoreDefaultToTheSandbox(t *testing.T) {
	launch, err := BuildLaunchCommand(LaunchConfig{Harness: HarnessCodex, Binary: "codex", WorkspacePath: "/tmp/wt"})
	if err != nil {
		t.Fatal(err)
	}
	restore, ok, err := BuildRestoreCommand(RestoreConfig{Harness: HarnessCodex, Binary: "codex", WorkspacePath: "/tmp/wt",
		Metadata: map[string]string{MetadataKeyAgentSessionID: "s-1"}})
	if err != nil || !ok {
		t.Fatalf("restore command: ok=%v err=%v", ok, err)
	}
	for name, cmd := range map[string][]string{"launch": launch, "restore": restore} {
		if slices.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
			t.Fatalf("%s carries the bypass: %v", name, cmd)
		}
		if !slices.Contains(cmd, "workspace-write") {
			t.Fatalf("%s lacks the workspace-write sandbox: %v", name, cmd)
		}
	}
}
