// Command ao-web-dast is AO's own active-pentest checker. It runs inside the
// hardened skill container, reachable to exactly one authorized target through
// the staged egress proxy. It reads a JSON config from a mounted file and
// writes an ao.pentest/v1 report to stdout.
//
// It holds no AO credentials, takes nothing from the daemon's environment
// beyond HTTP(S)_PROXY, and makes only safe, non-destructive requests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/webdast"
)

func main() {
	configPath := flag.String("config", "", "path to the JSON run config (required)")
	controls := flag.String("controls", "", "comma-separated runner controls, recorded in provenance")
	flag.Parse()

	if strings.TrimSpace(*configPath) == "" {
		fmt.Fprintln(os.Stderr, "ao-web-dast: -config is required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*configPath) //nolint:gosec // the config path is the one AO staged read-only.
	if err != nil {
		fmt.Fprintf(os.Stderr, "ao-web-dast: read config: %v\n", err)
		os.Exit(2)
	}
	var cfg webdast.Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ao-web-dast: parse config: %v\n", err)
		os.Exit(2)
	}

	var runnerControls []string
	if strings.TrimSpace(*controls) != "" {
		for _, c := range strings.Split(*controls, ",") {
			if c = strings.TrimSpace(c); c != "" {
				runnerControls = append(runnerControls, c)
			}
		}
	}

	report, err := webdast.Run(context.Background(), cfg, runnerControls)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ao-web-dast: %v\n", err)
		os.Exit(1)
	}
	out, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ao-web-dast: marshal report: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		os.Exit(1)
	}
}
