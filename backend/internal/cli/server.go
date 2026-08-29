package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemon"
)

type serverOptions struct {
	host    string
	port    int
	dataDir string
	webRoot string
}

func newServerCommand() *cobra.Command {
	opts := serverOptions{}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run AO in foreground with its browser UI",
		Long: "Run the real AO daemon in foreground and serve the compiled React UI.\n\n" +
			"Headless web mode is local-only and has no production authentication.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := serverConfig(opts)
			if err != nil {
				return usageError{err}
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "AO headless web server: http://%s\n", cfg.Addr())
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Data directory: %s\nFrontend assets: %s\n", cfg.DataDir, cfg.WebRoot)
			return daemon.RunWithConfig(cfg)
		},
	}
	cmd.Flags().StringVar(&opts.host, "host", config.LoopbackHost, "Bind host (8G supports loopback only)")
	cmd.Flags().IntVar(&opts.port, "port", config.DefaultPort, "HTTP port")
	cmd.Flags().StringVar(&opts.dataDir, "data-dir", "", "Durable data directory (default ~/.ao/data)")
	cmd.Flags().StringVar(&opts.webRoot, "web-root", "", "Compiled web assets (auto-detected when omitted)")
	return cmd
}

func serverConfig(opts serverOptions) (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	if ip := net.ParseIP(opts.host); ip == nil || !ip.IsLoopback() || opts.host != config.LoopbackHost {
		return config.Config{}, fmt.Errorf("--host must be %s; remote exposure is reserved for an authenticated listener", config.LoopbackHost)
	}
	if opts.port < 1 || opts.port > 65535 {
		return config.Config{}, fmt.Errorf("--port must be in range 1-65535")
	}
	cfg.Host = opts.host
	cfg.Port = opts.port
	if opts.dataDir != "" {
		cfg.DataDir, err = filepath.Abs(opts.dataDir)
		if err != nil {
			return config.Config{}, fmt.Errorf("resolve --data-dir: %w", err)
		}
		cfg.RunFilePath = filepath.Join(cfg.DataDir, "running.json")
	}
	cfg.WebRoot, err = resolveWebRoot(opts.webRoot)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func resolveWebRoot(explicit string) (string, error) {
	if explicit != "" {
		return validateWebRoot(explicit)
	}
	cwd, _ := os.Getwd()
	exe, _ := os.Executable()
	candidates := []string{
		filepath.Join(cwd, "frontend", "dist", "web"),
		filepath.Join(cwd, "..", "frontend", "dist", "web"),
		filepath.Join(filepath.Dir(exe), "web"),
	}
	for _, candidate := range candidates {
		if root, err := validateWebRoot(candidate); err == nil {
			return root, nil
		}
	}
	return "", fmt.Errorf("compiled frontend not found; run `npm --prefix frontend run build:web` or pass --web-root")
}

func validateWebRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve web root: %w", err)
	}
	info, err := os.Stat(filepath.Join(abs, "index.html"))
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("web root %s does not contain index.html", abs)
	}
	return abs, nil
}
