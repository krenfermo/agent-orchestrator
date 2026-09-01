package cli

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// memory_integrity.go — the P2-D diagnostics (§27).
//
// Two commands, answering the two questions an operator actually arrives with:
//
//	ao memory validate <project>   which facts can AO still prove, and why not
//	ao memory provenance <project> <item>   where did THIS fact come from
//
// They are separate from `ao memory invalidate` on purpose, and the separation
// is the same one the subsystem rests on. `invalidate` (and the drift check it
// runs with no --path) asks whether a fact's SOURCES still look the same;
// `validate` asks whether the thing that made the fact the project's knowledge
// is still on record. An operator seeing "12 facts drifted" and an operator
// seeing "12 facts have no provable promotion" have entirely different next
// steps, and one command reporting both would have to pick one word for two
// problems.
//
// `validate` defaults to a DRY RUN. An integrity diagnostic that changed
// things by default is one an operator hesitates to run, and hesitating to run
// the integrity check is the worst possible outcome for it.

type memoryValidateEnvelope struct {
	RepoID           string `json:"repoId"`
	RepoIdentity     string `json:"repoIdentity"`
	Applied          bool   `json:"applied"`
	Checked          int    `json:"checked"`
	Provable         int    `json:"provable"`
	IdentityWithheld int64  `json:"identityWithheld"`
	LegacyClassified int64  `json:"legacyClassified"`
	EdgesRetired     int64  `json:"edgesRetired"`
	Truncated        bool   `json:"truncated"`
	Findings         []struct {
		ItemID      string `json:"itemId"`
		Type        string `json:"type"`
		Scope       string `json:"scope"`
		Key         string `json:"key"`
		From        string `json:"from"`
		To          string `json:"to"`
		ReasonClass string `json:"reasonClass"`
		Detail      string `json:"detail"`
		Applied     bool   `json:"applied"`
	} `json:"findings"`
}

type memoryProvenanceEnvelope struct {
	Item struct {
		ID                 string   `json:"id"`
		RepoID             string   `json:"repoId"`
		Type               string   `json:"type"`
		Scope              string   `json:"scope"`
		Key                string   `json:"key"`
		Origin             string   `json:"origin"`
		OriginRef          string   `json:"originRef"`
		Summary            string   `json:"summary"`
		State              string   `json:"state"`
		StateReason        string   `json:"stateReason"`
		Generation         int64    `json:"generation"`
		SourceCommit       string   `json:"sourceCommit"`
		SourcePaths        []string `json:"sourcePaths"`
		Authority          string   `json:"authority"`
		AuthorityReason    string   `json:"authorityReason"`
		Servable           bool     `json:"servable"`
		ProvenanceKind     string   `json:"provenanceKind"`
		RepoIdentity       string   `json:"repoIdentity"`
		PromotionAuthority string   `json:"promotionAuthority"`
		VerifiedCommit     string   `json:"verifiedCommit"`
		IntegratedCommit   string   `json:"integratedCommit"`
		UpdatedAt          string   `json:"updatedAt"`
	} `json:"item"`
	Servable             bool   `json:"servable"`
	AuthorityReasonClass string `json:"authorityReasonClass"`
	Relations            []struct {
		Kind        string `json:"kind"`
		FromKind    string `json:"fromKind"`
		FromKey     string `json:"fromKey"`
		ToKind      string `json:"toKind"`
		ToKey       string `json:"toKey"`
		State       string `json:"state"`
		StateReason string `json:"stateReason"`
		Authority   string `json:"authority"`
	} `json:"relations"`
}

func newMemoryValidateCommand(ctx *commandContext) *cobra.Command {
	var repoPath string
	var apply bool
	var limit int64
	cmd := &cobra.Command{
		Use:   "validate <project-id>",
		Short: "Check which facts AO can still prove it is entitled to serve",
		Long: "Runs the authority pass: for every fact currently being served, AO checks that the thing which made it\n" +
			"the project's knowledge is still on record — the repository is still the same repository, the\n" +
			"promotion that made a task's fact canonical still has a mutation-provenance row behind it, and the\n" +
			"kind of proof the fact needs is one this build can check.\n\n" +
			"This is NOT drift detection. `ao memory invalidate` (with no --path) asks whether a fact's source\n" +
			"FILES still look the same; this asks whether its LICENCE still holds. A fact whose files nobody has\n" +
			"touched can lose its licence, and a fact whose files changed can keep one — so the two are reported\n" +
			"separately and repaired differently.\n\n" +
			"It is a dry run by default and only ever demotes. Nothing here can make a withheld fact authoritative\n" +
			"again: re-establishing a licence is a promotion or a rebuild, both of which are explicit acts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"repoPath": repoPath, "apply": apply}
			if limit > 0 {
				body["limit"] = limit
			}
			var res memoryValidateEnvelope
			if err := ctx.postJSON(cmd.Context(), "projects/"+url.PathEscape(args[0])+"/memory/validate", body, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			identity := res.RepoIdentity
			if strings.TrimSpace(identity) == "" {
				// Said plainly rather than left blank: an unidentifiable
				// repository is why several checks below can only ever be
				// inconclusive, and an operator should not have to work that
				// out from an empty field.
				identity = "(this checkout has no durable identity AO can read)"
			}
			_, _ = fmt.Fprintf(out, "repository %s identifies as %s\n", res.RepoID, identity)
			_, _ = fmt.Fprintf(out, "checked %d facts; %d still provable\n", res.Checked, res.Provable)
			if res.Truncated {
				_, _ = fmt.Fprintf(out, "the pass stopped at its limit, so facts beyond it were not checked\n")
			}
			if res.IdentityWithheld > 0 {
				_, _ = fmt.Fprintf(out,
					"%d facts were derived from a DIFFERENT repository at this path and are no longer served\n",
					res.IdentityWithheld)
			}
			if res.LegacyClassified > 0 {
				_, _ = fmt.Fprintf(out,
					"%d facts predate provenance recording; they are withheld and can be recovered with `ao memory rebuild`\n",
					res.LegacyClassified)
			}
			if res.EdgesRetired > 0 {
				_, _ = fmt.Fprintf(out, "%d graph edges were retired because a fact they name is no longer provable\n",
					res.EdgesRetired)
			}
			for _, f := range res.Findings {
				name := f.Type
				if f.Key != "" {
					name += " " + f.Key
				}
				_, _ = fmt.Fprintf(out, "  %s  %s -> %s  [%s] %s\n",
					name, f.From, f.To, f.ReasonClass, f.Detail)
				_, _ = fmt.Fprintf(out, "    %s (%s)\n", f.ItemID, appliedWord(f.Applied, apply))
			}
			if !apply && (len(res.Findings) > 0 || res.Checked > res.Provable) {
				_, _ = fmt.Fprintln(out, "nothing was changed; re-run with --apply to withhold these facts")
			}
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to the project's own root)")
	cmd.Flags().BoolVar(&apply, "apply", false, "Write the demotions instead of only reporting them")
	cmd.Flags().Int64Var(&limit, "limit", 0, "Maximum facts to check (defaults to 2000)")
	return cmd
}

func appliedWord(applied, requested bool) string {
	switch {
	case applied:
		return "withheld"
	case requested:
		// The store refused the write because a newer generation had already
		// moved the row. That is the stale-validator case and is not an error;
		// saying so is better than printing "withheld" for something that was
		// not.
		return "not withheld: a newer generation had already moved this fact"
	default:
		return "dry run"
	}
}

func newMemoryProvenanceCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provenance <project-id> <item-id>",
		Short: "Show the full evidence chain behind one fact",
		Long: "Answers, for one memory item: why is it valid (or not), which task produced it, which commit supports\n" +
			"it, whether it was born on a branch or in a worktree, how it became canonical, what invalidated it,\n" +
			"and what replaced it.\n\n" +
			"Retired edges are shown as well as current ones. A superseded decision's `supersedes` edge is by\n" +
			"definition not in the current graph, and it is usually the exact thing being looked for.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "projects/" + url.PathEscape(args[0]) + "/memory/provenance/" + url.PathEscape(args[1])
			var res memoryProvenanceEnvelope
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			it := res.Item
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "%s  %s\n", it.ID, it.Summary)
			_, _ = fmt.Fprintf(out, "  type            %s (%s%s)\n", it.Type, it.Scope, keySuffix(it.Key))
			_, _ = fmt.Fprintf(out, "  served now      %s\n", servableWord(res.Servable))
			_, _ = fmt.Fprintf(out, "  drift state     %s%s\n", it.State, reasonSuffix(it.StateReason))
			_, _ = fmt.Fprintf(out, "  authority       %s%s\n", it.Authority, reasonSuffix(it.AuthorityReason))
			_, _ = fmt.Fprintf(out, "  provenance      %s\n", orNoneWord(it.ProvenanceKind))
			_, _ = fmt.Fprintf(out, "  repository      %s\n", orNoneWord(it.RepoIdentity))
			_, _ = fmt.Fprintf(out, "  born as         %s%s\n", it.Origin, originRefSuffix(it.OriginRef))
			_, _ = fmt.Fprintf(out, "  generation      %d\n", it.Generation)
			_, _ = fmt.Fprintf(out, "  source commit   %s\n", orNoneWord(it.SourceCommit))
			_, _ = fmt.Fprintf(out, "  verified at     %s\n", orNoneWord(it.VerifiedCommit))
			_, _ = fmt.Fprintf(out, "  integrated at   %s\n", orNoneWord(it.IntegratedCommit))
			_, _ = fmt.Fprintf(out, "  promoted by     %s\n", orNoneWord(it.PromotionAuthority))
			if len(it.SourcePaths) > 0 {
				_, _ = fmt.Fprintf(out, "  evidence        %s\n", strings.Join(it.SourcePaths, ", "))
			}
			if len(res.Relations) > 0 {
				_, _ = fmt.Fprintln(out, "  related:")
				for _, rel := range res.Relations {
					_, _ = fmt.Fprintf(out, "    %s %s:%s -> %s:%s  [%s/%s]%s\n",
						rel.Kind, rel.FromKind, rel.FromKey, rel.ToKind, rel.ToKey,
						rel.State, rel.Authority, reasonSuffix(rel.StateReason))
				}
			}
			return nil
		},
	}
	return cmd
}

func servableWord(servable bool) string {
	if servable {
		return "yes — this fact is handed to agents as current"
	}
	return "no — this fact is withheld"
}

func orNoneWord(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(not recorded)"
	}
	return s
}

func keySuffix(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return " " + key
}

func reasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return " — " + reason
}

func originRefSuffix(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	return " of task " + ref
}
