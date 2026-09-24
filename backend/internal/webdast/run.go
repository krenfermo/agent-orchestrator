package webdast

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Run executes the active-pentest checks described by cfg and returns the
// ao.pentest/v1 report. It never returns an error for a target that simply
// behaves badly — a refused connection, a slow endpoint, a blocked off-target
// attempt are all recorded IN the report. It returns an error only for a config
// it cannot run at all, so the caller can distinguish "nothing to report" from
// "could not start".
//
// runnerControls is what the daemon attested the run executed under; it is
// copied into provenance so the report says how it ran, not only what it found.
func Run(ctx context.Context, cfg Config, runnerControls []string) (Report, error) {
	if err := cfg.Validate(); err != nil {
		return Report{}, err
	}
	limits := cfg.Limits.Normalize()

	c, err := newClient(cfg, limits)
	if err != nil {
		return Report{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, limits.Duration())
	defer cancel()

	started := time.Now().UTC()
	findings, checksRun := runChecks(runCtx, c, cfg)
	ended := time.Now().UTC()

	// Stable, deterministic finding ids: sort by check, severity, endpoint,
	// then assign PEN-1.. so two runs over the same target number alike.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Check != findings[j].Check {
			return findings[i].Check < findings[j].Check
		}
		if findings[i].Endpoint != findings[j].Endpoint {
			return findings[i].Endpoint < findings[j].Endpoint
		}
		return findings[i].Title < findings[j].Title
	})
	for i := range findings {
		findings[i].ID = fmt.Sprintf("PEN-%d", i+1)
	}
	if findings == nil {
		findings = []Finding{}
	}

	blockedAttempts, blockedRedirects, runtimeErrors := c.blocked()
	sort.Slice(blockedAttempts, func(i, j int) bool {
		return blockedAttempts[i].Destination < blockedAttempts[j].Destination
	})
	if blockedAttempts == nil {
		blockedAttempts = []BlockedAttempt{}
	}
	if blockedRedirects == nil {
		blockedRedirects = []BlockedRedirect{}
	}
	if runtimeErrors == nil {
		runtimeErrors = []string{}
	}

	endpoints := cfg.endpointPaths(limits)
	toolName := cfg.ToolName
	if toolName == "" {
		toolName = "ao-web-dast"
	}
	toolVersion := cfg.ToolVersion
	if toolVersion == "" {
		toolVersion = "1"
	}

	report := Report{
		SchemaVersion:   ReportSchemaVersion,
		ProjectID:       cfg.ProjectID,
		SkillID:         cfg.SkillID,
		SkillVersion:    cfg.SkillVersion,
		RunID:           cfg.RunID,
		AuthorizationID: cfg.AuthorizationID,
		Target:          cfg.Target,
		ScopePaths:      cfg.ScopePaths,
		StartedAt:       started.Format(time.RFC3339),
		EndedAt:         ended.Format(time.RFC3339),
		Tool:            ToolInfo{Name: toolName, Version: toolVersion},
		Limits:          limits,
		Coverage: Coverage{
			Statement:       coverageStatement(len(endpoints), checksRun, c.requestsMade()),
			EndpointsProbed: len(endpoints),
			ChecksRun:       checksRun,
			NotAttempted:    notAttempted,
		},
		RequestsMade:     c.requestsMade(),
		Findings:         findings,
		BlockedAttempts:  blockedAttempts,
		BlockedRedirects: blockedRedirects,
		RuntimeErrors:    runtimeErrors,
		Limitations: []string{
			"This checker runs a fixed set of safe, non-destructive checks. An empty findings list means these checks found nothing, not that the target is secure.",
			"Active injection, fuzzing and brute-force are deliberately not attempted in this version.",
			"The target is reached only through AO's egress proxy; anything the proxy blocked is listed under blockedAttempts.",
		},
		Provenance: Provenance{
			RequestedBy:      cfg.RequestedBy,
			AuthorizationRef: cfg.AuthorizationRef,
			RunnerControls:   runnerControls,
		},
	}
	if report.RuntimeErrors == nil {
		report.RuntimeErrors = []string{}
	}
	return report, nil
}

func coverageStatement(endpoints int, checks []checkName, requests int) string {
	return fmt.Sprintf("ran %d checks over %d endpoint(s) in %d request(s), within the effective limits",
		len(checks), endpoints, requests)
}
