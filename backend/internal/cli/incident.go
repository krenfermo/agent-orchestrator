package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// incident.go — Checkpoint 8P-E.19's CLI surface for the Incident Advisor.
//
// Two commands, and they are on opposite sides of the trust boundary:
//
//	ao incident diagnose <workflow-id>   an operator asks AO to investigate
//	ao incident submit   --run <id> ...  the Diagnostic Agent delivers its answer
//
// `submit` is the ONLY write an isolated diagnostic agent is given, and it is
// the reason the agent needs no file-writing tool at all: the diagnosis travels
// as one JSON argument on this command rather than as a file the agent had to
// create. That is what lets the launcher deny Write outright.
//
// Neither command can execute an action. Executing is a separate endpoint with
// a separate approval, and there is deliberately no CLI shortcut to it — the
// consent for anything beyond the ordinary continue path is a human act in the
// UI, not a flag an agent could learn to pass.

type incidentResponseEnvelope struct {
	Incident struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Diagnosis *struct {
			Class   string `json:"classification"`
			Summary string `json:"summary"`
		} `json:"diagnosis"`
		ContextPack *struct {
			Bytes           int `json:"bytes"`
			EstimatedTokens int `json:"estimatedTokens"`
		} `json:"contextPack"`
	} `json:"incident"`
}

func newIncidentCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "incident",
		Short: "Investigate why an AO workflow run has stopped",
	}
	cmd.AddCommand(newIncidentDiagnoseCommand(ctx))
	cmd.AddCommand(newIncidentSubmitCommand(ctx))
	return cmd
}

func newIncidentDiagnoseCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagnose <workflow-id>",
		Short: "Ask AO to investigate a stopped workflow run with an isolated, read-only Diagnostic Agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := strings.TrimSpace(args[0])
			if runID == "" {
				return usageError{errors.New("usage: workflow id is required")}
			}
			var res incidentResponseEnvelope
			path := "workflows/" + url.PathEscape(runID) + "/incident/diagnose"
			if err := ctx.postJSON(cmd.Context(), path, struct{}{}, &res); err != nil {
				return err
			}
			pack := ""
			if res.Incident.ContextPack != nil {
				pack = fmt.Sprintf(" over a %d-byte pack (~%d tokens estimated)",
					res.Incident.ContextPack.Bytes, res.Incident.ContextPack.EstimatedTokens)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "incident %s is %s%s\n",
				res.Incident.ID, res.Incident.State, pack)
			return err
		},
	}
	return cmd
}

type incidentSubmitOptions struct {
	runID string
	file  string
	body  string
}

func newIncidentSubmitCommand(ctx *commandContext) *cobra.Command {
	var opts incidentSubmitOptions
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Record a Diagnostic Agent's classification and proposed action",
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.submitIncidentDiagnosis(cmd, opts)
		},
	}
	// Agents routinely spell flags with underscores; normalize both spellings,
	// mirroring decision.go and review.go.
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	cmd.Flags().StringVar(&opts.runID, "run", "", "Workflow run id the incident belongs to (required)")
	cmd.Flags().StringVar(&opts.body, "json", "", "The diagnosis as a single JSON object")
	cmd.Flags().StringVar(&opts.file, "file", "", "Path to a file containing the diagnosis JSON (alternative to --json)")
	return cmd
}

func (c *commandContext) submitIncidentDiagnosis(cmd *cobra.Command, opts incidentSubmitOptions) error {
	runID := strings.TrimSpace(opts.runID)
	if runID == "" {
		// Falls back to the launcher's env, so an agent that forgets the flag
		// still lands on its own incident rather than on someone else's.
		runID = strings.TrimSpace(os.Getenv("AO_INCIDENT_RUN_ID"))
	}
	if runID == "" {
		return usageError{errors.New("usage: --run is required")}
	}
	payload := strings.TrimSpace(opts.body)
	if payload == "" && strings.TrimSpace(opts.file) != "" {
		raw, err := os.ReadFile(opts.file)
		if err != nil {
			return fmt.Errorf("read diagnosis file: %w", err)
		}
		payload = strings.TrimSpace(string(raw))
	}
	if payload == "" {
		return usageError{errors.New("usage: one of --json or --file is required")}
	}
	// Parsed here purely to fail fast with a clear message: the daemon
	// validates the diagnosis properly (classification, missing evidence,
	// options, pack digest) and is the only thing that decides what is
	// acceptable.
	var probe map[string]any
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return usageError{fmt.Errorf("the diagnosis must be a single JSON object: %w", err)}
	}
	if probe["incidentId"] == nil || probe["incidentId"] == "" {
		if envID := strings.TrimSpace(os.Getenv("AO_INCIDENT_ID")); envID != "" {
			probe["incidentId"] = envID
		}
	}
	if probe["packDigest"] == nil || probe["packDigest"] == "" {
		if digest := strings.TrimSpace(os.Getenv("AO_INCIDENT_PACK_DIGEST")); digest != "" {
			probe["packDigest"] = digest
		}
	}

	var res incidentResponseEnvelope
	path := "workflows/" + url.PathEscape(runID) + "/incident/diagnosis"
	if err := c.postJSON(cmd.Context(), path, probe, &res); err != nil {
		return err
	}
	class := "recorded"
	if res.Incident.Diagnosis != nil {
		class = res.Incident.Diagnosis.Class
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "recorded diagnosis %s for incident %s (%s)\n",
		class, res.Incident.ID, res.Incident.State)
	return err
}
