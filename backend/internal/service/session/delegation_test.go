package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestDelegateTaskSpawnsWorkerThenRequestsTitleFromNewestActiveOrchestrator(t *testing.T) {
	tests := []struct {
		name      string
		agent     domain.AgentHarness
		model     string
		mode      domain.SessionMode
		wantAgent domain.AgentHarness
	}{
		{name: "project default"},
		{name: "requested agent model and mode", agent: domain.HarnessCursor, model: "  sonnet-custom  ", mode: domain.SessionModeChat, wantAgent: domain.HarnessCursor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
			now := time.Now().UTC()
			st.sessions["orch-old"] = domain.SessionRecord{ID: "orch-old", ProjectID: "ao", Kind: domain.KindOrchestrator, CreatedAt: now.Add(-time.Minute)}
			st.sessions["orch-new"] = domain.SessionRecord{ID: "orch-new", ProjectID: "ao", Kind: domain.KindOrchestrator, CreatedAt: now}
			st.sessions["orch-exited"] = domain.SessionRecord{ID: "orch-exited", ProjectID: "ao", Kind: domain.KindOrchestrator, Activity: domain.Activity{State: domain.ActivityExited}, CreatedAt: now.Add(time.Minute)}
			st.sessions["orch-dead"] = domain.SessionRecord{ID: "orch-dead", ProjectID: "ao", Kind: domain.KindOrchestrator, IsTerminated: true, CreatedAt: now.Add(2 * time.Minute)}
			st.sessions["worker"] = domain.SessionRecord{ID: "worker", ProjectID: "ao", Kind: domain.KindWorker, CreatedAt: now.Add(3 * time.Minute)}
			cmd := &fakeCommander{}
			svc := &Service{store: st, manager: cmd, runBackground: runInline}

			brief := "  Fix the renderer\nwithout changing the API.  "
			out, err := svc.DelegateTask(context.Background(), DelegateTaskInput{
				ProjectID: "ao", Brief: brief, RequestedAgent: tt.agent, Model: tt.model, RequestedMode: tt.mode,
			})
			if err != nil {
				t.Fatalf("DelegateTask: %v", err)
			}
			if out.WorkerID != "mer-9" || out.OrchestratorID != "" {
				t.Fatalf("out = %#v, want worker mer-9 with asynchronous title handoff", out)
			}
			if !cmd.spawned || cmd.spawnedCfg.ProjectID != "ao" || cmd.spawnedCfg.Kind != domain.KindWorker || cmd.spawnedCfg.Harness != tt.wantAgent || cmd.spawnedCfg.Prompt != brief || cmd.spawnedCfg.DisplayName != "Fix the renderer wit" {
				t.Fatalf("spawn cfg = %#v", cmd.spawnedCfg)
			}
			if cmd.spawnedCfg.AgentConfig.Model != strings.TrimSpace(tt.model) {
				t.Fatalf("spawn model = %q, want %q", cmd.spawnedCfg.AgentConfig.Model, strings.TrimSpace(tt.model))
			}
			if cmd.spawnedCfg.RequestedMode != tt.mode {
				t.Fatalf("spawn mode = %q, want %q", cmd.spawnedCfg.RequestedMode, tt.mode)
			}
			if len(cmd.sent) != 1 || cmd.sent[0] != "orch-new" {
				t.Fatalf("sent = %#v; want orch-new", cmd.sent)
			}
			if len(cmd.ready) != 1 || cmd.ready[0] != "orch-new" {
				t.Fatalf("readiness waits = %#v; want orch-new", cmd.ready)
			}
			for _, want := range []string{
				"AO TASK TITLE UPDATE",
				"Do not spawn another worker or orchestrator",
				`ao session rename mer-9 "<title, max 20 chars>"`,
				"Worker session id: mer-9",
				brief,
			} {
				if !strings.Contains(cmd.sentMessages[0], want) {
					t.Fatalf("title delegation missing %q:\n%s", want, cmd.sentMessages[0])
				}
			}
			if tt.model != "" && !strings.Contains(cmd.sentMessages[0], "Requested model: sonnet-custom") {
				t.Fatalf("title delegation missing requested model:\n%s", cmd.sentMessages[0])
			}
		})
	}
}

func TestDelegatedTaskDisplayName(t *testing.T) {
	for _, tt := range []struct {
		name  string
		brief string
		want  string
	}{
		{name: "empty", brief: " \n\t ", want: "Untitled task"},
		{name: "short", brief: "  tell me a joke  ", want: "tell me a joke"},
		{name: "whitespace", brief: "Fix the renderer\nwithout changing the API", want: "Fix the renderer wit"},
		{name: "unicode rune limit", brief: "一二三四五六七八九十一二三四五六七八九十一", want: "一二三四五六七八九十一二三四五六七八九十"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := delegatedTaskDisplayName(tt.brief); got != tt.want {
				t.Fatalf("delegatedTaskDisplayName(%q) = %q, want %q", tt.brief, got, tt.want)
			}
		})
	}
}

func TestDelegateTaskStartsPromptlessWorkerWithoutRequestingTitle(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	st.sessions["orch"] = domain.SessionRecord{ID: "orch", ProjectID: "ao", Kind: domain.KindOrchestrator}
	cmd := &fakeCommander{}

	out, err := (&Service{store: st, manager: cmd, runBackground: runInline}).DelegateTask(
		context.Background(),
		DelegateTaskInput{ProjectID: "ao", Brief: " \n\t "},
	)
	if err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}
	if out.WorkerID != "mer-9" || out.OrchestratorID != "" {
		t.Fatalf("out = %#v, want promptless worker mer-9", out)
	}
	if !cmd.spawned || cmd.spawnedCfg.Prompt != "" || cmd.spawnedCfg.DisplayName != "Untitled task" {
		t.Fatalf("spawn cfg = %#v", cmd.spawnedCfg)
	}
	if len(cmd.ready) != 0 || len(cmd.sent) != 0 || len(cmd.resumed) != 0 {
		t.Fatalf("promptless spawn contacted orchestrator: ready=%#v sent=%#v resumed=%#v", cmd.ready, cmd.sent, cmd.resumed)
	}
}

func TestDelegateTaskResumesNewestExitedOrchestratorBeforeRequestingTitle(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	now := time.Now().UTC()
	st.sessions["orch-old"] = domain.SessionRecord{ID: "orch-old", ProjectID: "ao", Kind: domain.KindOrchestrator, Activity: domain.Activity{State: domain.ActivityExited}, CreatedAt: now.Add(-time.Minute)}
	st.sessions["orch-new"] = domain.SessionRecord{ID: "orch-new", ProjectID: "ao", Kind: domain.KindOrchestrator, Activity: domain.Activity{State: domain.ActivityExited}, CreatedAt: now}
	cmd := &fakeCommander{}

	out, err := (&Service{store: st, manager: cmd, runBackground: runInline}).DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ao", Brief: "Fix it"})
	if err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}
	if out.WorkerID != "mer-9" || out.OrchestratorID != "" {
		t.Fatalf("out = %#v, want worker mer-9 with asynchronous title handoff", out)
	}
	if len(cmd.resumed) != 1 || cmd.resumed[0] != "orch-new" {
		t.Fatalf("resumed = %#v, want orch-new", cmd.resumed)
	}
	if len(cmd.ready) != 1 || cmd.ready[0] != "orch-new" {
		t.Fatalf("readiness waits = %#v, want orch-new", cmd.ready)
	}
	if len(cmd.sent) != 1 || cmd.sent[0] != "orch-new" {
		t.Fatalf("sent = %#v, want orch-new", cmd.sent)
	}
}

func TestDelegateTaskStartsMissingOrchestratorBeforeRequestingTitle(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	st.sessions["orch-dead"] = domain.SessionRecord{ID: "orch-dead", ProjectID: "ao", Kind: domain.KindOrchestrator, IsTerminated: true}
	cmd := &fakeCommander{spawnFunc: func(cfg ports.SpawnConfig) domain.SessionRecord {
		if cfg.Kind == domain.KindOrchestrator {
			return domain.SessionRecord{ID: "orch-new", ProjectID: cfg.ProjectID, Kind: cfg.Kind}
		}
		return domain.SessionRecord{ID: "worker-new", ProjectID: cfg.ProjectID, Kind: cfg.Kind}
	}}

	out, err := (&Service{store: st, manager: cmd, runBackground: runInline}).DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ao", Brief: "Fix it"})
	if err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}
	if out.WorkerID != "worker-new" || out.OrchestratorID != "" {
		t.Fatalf("out = %#v, want worker-new with asynchronous title handoff", out)
	}
	if cmd.spawnCalls != 2 {
		t.Fatalf("spawn calls = %d, want worker plus orchestrator", cmd.spawnCalls)
	}
	if len(cmd.ready) != 1 || cmd.ready[0] != "orch-new" {
		t.Fatalf("readiness waits = %#v, want orch-new", cmd.ready)
	}
	if len(cmd.sent) != 1 || cmd.sent[0] != "orch-new" {
		t.Fatalf("sent = %#v, want orch-new", cmd.sent)
	}
}

func TestDelegateTaskKeepsSpawnSuccessWhenTitleOrchestratorNeverBecomesReady(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	st.sessions["orch"] = domain.SessionRecord{ID: "orch", ProjectID: "ao", Kind: domain.KindOrchestrator}
	cmd := &fakeCommander{readyErr: errors.New("readiness timed out")}

	out, err := (&Service{store: st, manager: cmd, runBackground: runInline}).DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ao", Brief: "Fix it"})
	if err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}
	if out.WorkerID != "mer-9" || out.OrchestratorID != "" {
		t.Fatalf("out = %#v, want spawned worker without title recipient", out)
	}
	if len(cmd.ready) != 1 || cmd.ready[0] != "orch" {
		t.Fatalf("readiness waits = %#v, want orch", cmd.ready)
	}
	if len(cmd.sent) != 0 {
		t.Fatalf("sent = %#v, want no title request before readiness", cmd.sent)
	}
}

func TestDelegateTaskKeepsSpawnSuccessWhenTitleRequestFails(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	st.sessions["orch"] = domain.SessionRecord{ID: "orch", ProjectID: "ao", Kind: domain.KindOrchestrator}
	cmd := &fakeCommander{sendErr: errors.New("orchestrator exited")}

	out, err := (&Service{store: st, manager: cmd, runBackground: runInline}).DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ao", Brief: "Fix it"})
	if err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}
	if out.WorkerID != "mer-9" || out.OrchestratorID != "" {
		t.Fatalf("out = %#v, want spawned worker without title recipient", out)
	}
	if !cmd.spawned {
		t.Fatal("worker was not spawned")
	}
}

func TestDelegateTaskReturnsBeforeTitleRequestCompletes(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	st.sessions["orch"] = domain.SessionRecord{ID: "orch", ProjectID: "ao", Kind: domain.KindOrchestrator}
	titleStarted := make(chan struct{})
	releaseTitle := make(chan struct{})
	titleFinished := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseTitle:
		default:
			close(releaseTitle)
		}
	})
	cmd := &fakeCommander{sendFunc: func(domain.SessionID, string) error {
		close(titleStarted)
		<-releaseTitle
		close(titleFinished)
		return nil
	}}
	svc := &Service{store: st, manager: cmd}

	type result struct {
		out DelegateTaskOutcome
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		out, err := svc.DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ao", Brief: "Fix it"})
		resultCh <- result{out: out, err: err}
	}()

	select {
	case got := <-resultCh:
		if got.err != nil || got.out.WorkerID != "mer-9" {
			t.Fatalf("DelegateTask = %#v, %v", got.out, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DelegateTask waited for background title request")
	}
	select {
	case <-titleStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background title request did not start")
	}
	close(releaseTitle)
	select {
	case <-titleFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("background title request did not finish")
	}
}

func runInline(work func()) {
	work()
}

// TestDelegateTaskSendsTheWorkerTheWholeSpecificationButTheTitleAgentAnExcerpt
// is the context-window half of raising the brief ceiling to 128 KiB.
//
// The worker is the one doing the work and must receive the specification
// entire. The orchestrator is only being asked to pick a <=20 character title,
// and sending it the whole thing a second time would spend a context window on
// a naming errand.
func TestDelegateTaskSendsTheWorkerTheWholeSpecificationButTheTitleAgentAnExcerpt(t *testing.T) {
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao"}
	st.sessions["orch"] = domain.SessionRecord{ID: "orch", ProjectID: "ao", Kind: domain.KindOrchestrator, CreatedAt: time.Now().UTC()}
	cmd := &fakeCommander{}
	svc := &Service{store: st, manager: cmd, runBackground: runInline}

	// Multibyte on purpose: the excerpt is cut on a byte budget, so it must
	// stop on a rune boundary rather than halfway through a character.
	brief := "Implementar RBAC en MEDUSA.\n" + strings.Repeat("especificación detallada ñ\n", 4000)
	if len(brief) <= delegatedTaskTitleBriefBytes {
		t.Fatalf("fixture is not long enough to exercise the excerpt: %d bytes", len(brief))
	}

	if _, err := svc.DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ao", Brief: brief}); err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}

	if cmd.spawnedCfg.Prompt != brief {
		t.Fatalf("the worker received %d bytes of a %d byte specification — it must get all of it",
			len(cmd.spawnedCfg.Prompt), len(brief))
	}
	msg := cmd.sentMessages[0]
	if len(msg) > delegatedTaskTitleBriefBytes*2 {
		t.Fatalf("the title request carries %d bytes; the excerpt cap is %d",
			len(msg), delegatedTaskTitleBriefBytes)
	}
	if !strings.Contains(msg, "Implementar RBAC en MEDUSA.") {
		t.Fatalf("the excerpt dropped the opening line, which is what names the task:\n%s", msg)
	}
	if !strings.Contains(msg, "the worker has the full specification") {
		t.Fatalf("the title request does not say the brief is an excerpt:\n%s", msg)
	}
	if !utf8.ValidString(msg) {
		t.Fatal("the excerpt was cut through the middle of a multibyte character")
	}
}

// TestBriefExcerptLeavesAShortBriefAlone keeps the excerpt invisible for the
// ordinary case: a normal one-line brief is passed through untouched and
// unannotated.
func TestBriefExcerptLeavesAShortBriefAlone(t *testing.T) {
	brief := "Arreglar el botón de pago"
	got, truncated := briefExcerpt(brief, delegatedTaskTitleBriefBytes)
	if truncated || got != brief {
		t.Fatalf("briefExcerpt(%q) = %q, %v; want the brief unchanged", brief, got, truncated)
	}
}
