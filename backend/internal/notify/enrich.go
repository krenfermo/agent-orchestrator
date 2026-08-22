package notify

import (
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func enrich(intent Intent) (domain.NotificationRecord, error) {
	rec := domain.NotificationRecord{
		SessionID:     intent.SessionID,
		ProjectID:     intent.ProjectID,
		WorkflowRunID: strings.TrimSpace(intent.WorkflowRunID),
		DedupeKey:     strings.TrimSpace(intent.DedupeKey),
		PRURL:         strings.TrimSpace(intent.PRURL),
		Type:          intent.Type,
		Status:        domain.NotificationUnread,
		CreatedAt:     intent.CreatedAt,
	}
	if !intent.Type.Valid() {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationType
	}
	// The PR-outcome family is meaningless without the PR it is about. The two
	// session/run-scoped families (needs-input, completion) carry no PR at all.
	if !intent.Type.Completion() && intent.Type != domain.NotificationNeedsInput && rec.PRURL == "" {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationRecord
	}
	// A completion is a one-off event, and the key naming that event is what
	// makes it un-duplicatable. Producing one without a key would quietly fall
	// back to open-row dedupe, which lets a restart re-announce work the user
	// already saw finish — exactly the failure this family must not have.
	if intent.Type.Completion() && rec.DedupeKey == "" {
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
	default:
		return ""
	}
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

func prLabel(intent Intent) string {
	if intent.PRNumber > 0 {
		return fmt.Sprintf("PR #%d", intent.PRNumber)
	}
	if title := strings.TrimSpace(intent.PRTitle); title != "" {
		return "PR " + title
	}
	return "PR"
}
