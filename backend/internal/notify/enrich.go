package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func enrich(intent Intent) (domain.NotificationRecord, error) {
	rec := domain.NotificationRecord{
		SessionID:     intent.SessionID,
		ProjectID:     intent.ProjectID,
		WorkflowRunID: strings.TrimSpace(intent.WorkflowRunID),
		DedupeKey:     strings.TrimSpace(intent.DedupeKey),
		PRURL:         strings.TrimSpace(intent.PRURL),
		TaskID:        strings.TrimSpace(intent.TaskID),
		Type:          intent.Type,
		Status:        domain.NotificationUnread,
		CreatedAt:     intent.CreatedAt,
		Source:        intent.Source,
		SourceEventID: strings.TrimSpace(intent.SourceEventID),
	}
	// Every intent that does not name its producer came from lifecycle, which
	// is where they all came from before provenance was recorded.
	if rec.Source == "" {
		rec.Source = domain.NotificationSourceLifecycle
	}
	if !intent.Type.Valid() {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationType
	}
	// The PR-outcome family is meaningless without the PR it is about. The
	// session/run-scoped families (needs-input, and every event-keyed family)
	// carry no PR at all.
	if !intent.Type.EventKeyed() && intent.Type != domain.NotificationNeedsInput && rec.PRURL == "" {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationRecord
	}
	// An event-keyed notification is a one-off fact, and the key naming that
	// event is what makes it un-duplicatable. Producing one without a key would
	// quietly fall back to open-row dedupe, which lets a restart re-announce
	// something the user already saw — exactly the failure these families must
	// not have.
	if intent.Type.EventKeyed() && rec.DedupeKey == "" {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationRecord
	}
	rec.Title = titleForIntent(intent)
	rec.Body = bodyForIntent(intent)
	if err := rec.Validate(); err != nil {
		return domain.NotificationRecord{}, err
	}
	return rec, nil
}

func titleForIntent(intent Intent) string {
	switch intent.Type {
	case domain.NotificationNeedsInput:
		return fmt.Sprintf("%s needs your input", sessionLabel(intent))
	case domain.NotificationReadyToMerge:
		if title := strings.TrimSpace(intent.PRTitle); title != "" {
			if label := prLabel(intent); label != "PR" {
				return fmt.Sprintf("%s · %s", title, label)
			}
			return title
		}
		return fmt.Sprintf("%s is ready to merge", prLabel(intent))
	case domain.NotificationPRMerged:
		return fmt.Sprintf("%s merged", prLabel(intent))
	case domain.NotificationPRClosedUnmerged:
		return fmt.Sprintf("%s closed", prLabel(intent))
	case domain.NotificationTaskCompleted:
		return fmt.Sprintf("%s finished", sessionLabel(intent))
	case domain.NotificationWorkflowCompleted:
		return fmt.Sprintf("%s finished", workflowLabel(intent))
	case domain.NotificationTaskNeedsAttention:
		return fmt.Sprintf("%s needs your attention", taskLabel(intent))
	case domain.NotificationWorkflowNeedsAttention:
		return fmt.Sprintf("%s needs your attention", workflowLabel(intent))
	case domain.NotificationTaskFailed:
		return fmt.Sprintf("%s failed", taskLabel(intent))
	case domain.NotificationWorkflowFailed:
		return fmt.Sprintf("%s failed", workflowLabel(intent))
	case domain.NotificationHumanQuestionRequired:
		return fmt.Sprintf("%s is waiting on your decision", sessionLabel(intent))
	case domain.NotificationRepairExhausted:
		return fmt.Sprintf("AO stopped retrying on %s", sessionLabel(intent))
	case domain.NotificationIntegrationFailed:
		return fmt.Sprintf("%s hit an integration failure", sessionLabel(intent))
	default:
		return "Notification"
	}
}

func bodyForIntent(intent Intent) string {
	switch intent.Type {
	case domain.NotificationNeedsInput:
		return "Your agent is waiting on you to continue."
	case domain.NotificationReadyToMerge:
		if session := sessionLabel(intent); session != "session" {
			return fmt.Sprintf("PR from session %s is ready to merge. CI passed with no blocking review feedback.", session)
		}
		return "CI passed with no blocking review feedback."
	case domain.NotificationPRMerged:
		title := strings.TrimSpace(intent.PRTitle)
		if target := strings.TrimSpace(intent.PRTargetBranch); title != "" && target != "" {
			return fmt.Sprintf("%s is now on %s.", title, target)
		}
		if title != "" {
			return fmt.Sprintf("%s was merged.", title)
		}
		return "The pull request was merged."
	case domain.NotificationPRClosedUnmerged:
		if title := strings.TrimSpace(intent.PRTitle); title != "" {
			return fmt.Sprintf("%s was closed without merging. Reopen it if this wasn't intended.", title)
		}
		return "Closed without merging. Reopen it if this wasn't intended."
	case domain.NotificationTaskCompleted:
		return "The task reported that it finished the work it was given."
	case domain.NotificationWorkflowCompleted:
		return "Every task in this workflow run completed."
	case domain.NotificationTaskNeedsAttention, domain.NotificationWorkflowNeedsAttention:
		return stopBody("It stopped and cannot continue without a decision from you.", intent)
	case domain.NotificationTaskFailed, domain.NotificationWorkflowFailed:
		return stopBody("It ended without completing the work it was given.", intent)
	case domain.NotificationHumanQuestionRequired:
		return stopBody(
			"The agent is stopped on a permission or approval prompt. "+
				"Only you can answer it: AO will not type into a blocked session.",
			intent,
		)
	case domain.NotificationRepairExhausted:
		return stopBody(
			"AO retried this automatically until the attempt budget ran out, "+
				"and the problem is still there.",
			intent,
		)
	case domain.NotificationIntegrationFailed:
		return stopBody("An integration AO runs for this session failed.", intent)
	default:
		return ""
	}
}

// stopBody states what happened, then whatever AO can say about why. Both
// carriers are optional: a stop AO could not name still produces a usable
// notification rather than none at all.
func stopBody(lead string, intent Intent) string {
	body := lead
	if detail := strings.TrimSpace(intent.Detail); detail != "" {
		body += "\n\n" + detail
	}
	if reason := strings.TrimSpace(intent.AttentionReason); reason != "" {
		body += "\n\nReason: " + reason
	}
	// Checkpoint 8P-E.24: a notification a person reads hours later has to be
	// actionable on its own. Two overnight blockages went unreported, and the
	// message that finally reported them has to say which project and which run
	// to open — a reason with no address is a reason nobody can act on.
	if project := strings.TrimSpace(string(intent.ProjectID)); project != "" {
		body += "\nProject: " + project
	}
	if runID := strings.TrimSpace(intent.WorkflowRunID); runID != "" {
		body += "\nRun: " + runID
	}
	if !intent.CreatedAt.IsZero() {
		body += "\nStopped at: " + intent.CreatedAt.UTC().Format(time.RFC3339)
	}
	return body
}

func sessionLabel(intent Intent) string {
	if v := strings.TrimSpace(intent.SessionDisplayName); v != "" {
		return v
	}
	if intent.SessionID != "" {
		return string(intent.SessionID)
	}
	return "session"
}

// workflowLabel prefers the objective a human wrote over the run id nobody
// recognizes, and falls back to the id rather than to a generic word.
func workflowLabel(intent Intent) string {
	if v := strings.TrimSpace(intent.WorkflowObjective); v != "" {
		return v
	}
	if v := strings.TrimSpace(intent.WorkflowRunID); v != "" {
		return "Workflow " + v
	}
	return "Workflow"
}

// taskLabel names a planned task's run the way the Board does: the first line
// of its objective, which is the planner's task title verbatim — a child run's
// objective is composed as title, blank line, description, and then a recap of
// each completed dependency task. Using the whole thing would put several
// paragraphs in a notification title and an email subject.
func taskLabel(intent Intent) string {
	if line := firstLine(intent.WorkflowObjective); line != "" {
		return line
	}
	if v := strings.TrimSpace(intent.WorkflowRunID); v != "" {
		return "Task " + v
	}
	return "Task"
}

// maxLabelRunes keeps a title readable in a toast, a notification row, and a
// mail subject line. Planner titles are short; a pasted objective is not.
const maxLabelRunes = 120

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if runes := []rune(trimmed); len(runes) > maxLabelRunes {
			return strings.TrimSpace(string(runes[:maxLabelRunes])) + "…"
		}
		return trimmed
	}
	return ""
}

func prLabel(intent Intent) string {
	if intent.PRNumber > 0 {
		return fmt.Sprintf("PR #%d", intent.PRNumber)
	}
	if title := strings.TrimSpace(intent.PRTitle); title != "" {
		return "PR " + title
	}
	return "PR"
}
