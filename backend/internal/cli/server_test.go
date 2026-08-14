package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

func TestServerConfigDefaultsToLoopbackAndRespectsOptions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "state")
	cfg, err := serverConfig(serverOptions{
		host: config.LoopbackHost, port: 43127, dataDir: dataDir, webRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != config.LoopbackHost || cfg.Port != 43127 {
		t.Fatalf("address = %s, want %s:43127", cfg.Addr(), config.LoopbackHost)
	}
	if cfg.DataDir != dataDir || cfg.RunFilePath != filepath.Join(dataDir, "running.json") {
		t.Fatalf("data/run paths = %q / %q", cfg.DataDir, cfg.RunFilePath)
	}
}

func TestServerConfigRejectsNonLoopbackHost(t *testing.T) {
	_, err := serverConfig(serverOptions{host: "0.0.0.0", port: 3001, webRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected non-loopback host rejection")
	}
}

func TestValidateWebRootRequiresIndex(t *testing.T) {
	if _, err := validateWebRoot(t.TempDir()); err == nil {
		t.Fatal("expected missing index.html error")
	}
}
