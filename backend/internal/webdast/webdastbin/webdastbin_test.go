package webdastbin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUnpackagedStoreFailsClosed(t *testing.T) {
	s := UnpackagedStoreForTest("arm64", []byte("checker-bytes"))
	if s.Packaged() {
		t.Fatalf("an unpackaged store must not report Packaged")
	}
	if _, _, err := s.Select("arm64"); err == nil {
		t.Fatalf("Select on an unpackaged store must fail closed")
	}
}

func TestStoreForTestSelectsVerifiedBytes(t *testing.T) {
	body := []byte("the checker binary bytes")
	s := StoreForTest(runtime.GOARCH, body)
	art, got, err := s.Select(runtime.GOARCH)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("select returned different bytes")
	}
	if art.SHA256 == "" || int(art.Bytes) != len(body) {
		t.Fatalf("artifact metadata wrong: %+v", art)
	}
	if _, _, err := s.Select("s390x"); err == nil {
		t.Fatalf("Select for an unpackaged arch must fail closed")
	}
}

func TestStageRoundTrip(t *testing.T) {
	body := []byte("checker")
	// A project whose parent is the temp dir; the staging root hangs off it.
	project := filepath.Join(t.TempDir(), "proj")
	root, err := StagingRootFor(project, "")
	if err != nil {
		t.Fatalf("staging root: %v", err)
	}
	staged, err := Stage(StageRequest{
		Store: StoreForTest("arm64", body), Root: root, RunID: "run-1",
		RuntimeArch: "aarch64", Config: []byte(`{"schemaVersion":"ao.webdast-config/v1"}`),
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := os.Stat(staged.BinaryPath); err != nil {
		t.Fatalf("binary not staged: %v", err)
	}
	if _, err := os.Stat(staged.ConfigPath); err != nil {
		t.Fatalf("config not staged: %v", err)
	}
	if staged.ContainerBinary() != ContainerDir+"/"+BinaryName || staged.ContainerConfig() != ContainerDir+"/"+ConfigName {
		t.Fatalf("container paths wrong: %s %s", staged.ContainerBinary(), staged.ContainerConfig())
	}
	if err := staged.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(staged.Dir); !os.IsNotExist(err) {
		t.Fatalf("staged dir should be gone after cleanup")
	}
}

func TestStageRefusesEmptyConfig(t *testing.T) {
	project := filepath.Join(t.TempDir(), "proj")
	root, _ := StagingRootFor(project, "")
	if _, err := Stage(StageRequest{
		Store: StoreForTest("arm64", []byte("x")), Root: root, RunID: "run-2", RuntimeArch: "aarch64", Config: nil,
	}); err == nil {
		t.Fatalf("Stage must refuse an empty config")
	}
}
