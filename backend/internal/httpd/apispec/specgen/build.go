// Package specgen builds the code-first OpenAPI document from the Go contract
// types. It lives outside apispec because it imports the controllers (to
// reflect their request/response shapes), and controllers import apispec (for
// the 501 stub) — keeping Build here breaks that cycle. apispec only embeds and
// serves the committed openapi.yaml; specgen produces it.
package specgen

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"

	jsonschema "github.com/swaggest/jsonschema-go"
	openapi "github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi31"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Build reflects the Go contract types and the operation registry below into
// the OpenAPI document. It is the single source of truth for the /api/v1
// contract: `cmd/genspec` writes its output to apispec/openapi.yaml (the
// committed, embedded artifact) and TestBuild_MatchesEmbedded asserts the embed
// equals fresh Build() output so the two can never drift. Schema facets live as
// struct tags on the service.*/controllers.* types; operation metadata (path,
// status codes, summaries) lives here.
//
// Every wire shape is reflected straight from where it is used at runtime — the
// request bodies, path params, and response envelopes from controllers, the
// error envelope from httpd/envelope — so the served responses and the
// generated schema share one definition each.
func Build() ([]byte, error) {
	r := openapi31.NewReflector()
	// Derive `required` from the idiomatic Go convention: a JSON field without
	// `omitempty` is required. swaggest does not infer this on its own, so the
	// structs stay clean (only description/enum tags) and this hook adds the
	// required array. nonNullableSlices drops the spurious "null" type swaggest
	// stamps on every Go slice.
	r.DefaultOptions = append(r.DefaultOptions,
		jsonschema.InterceptProp(requiredFromJSONTag),
		jsonschema.InterceptNullability(nonNullableSlices),
		// Clean component schema names (which become the generated TS type names):
		// swaggest defaults to PackageType, e.g. "ProjectProject", "EnvelopeAPIError".
		jsonschema.InterceptDefName(schemaName),
	)

	r.Spec.SetTitle("Agent Orchestrator HTTP daemon")
	r.Spec.SetVersion("0.1.0-route-shell")
	r.Spec.SetDescription("Loopback-only HTTP surface served by the Go daemon. " +
		"Generated from Go (code-first) — do not edit by hand; run `go generate ./...`.")
	r.Spec.Servers = []openapi31.Server{
		*(&openapi31.Server{URL: "http://127.0.0.1:3001"}).WithDescription("Local daemon (loopback only)"),
	}
	r.Spec.Tags = []openapi31.Tag{
		*(&openapi31.Tag{Name: "agents"}).WithDescription(
			"Supported and locally runnable agent adapters"),
		*(&openapi31.Tag{Name: "projects"}).WithDescription(
			"Project registry, configuration, and lifecycle administration"),
		*(&openapi31.Tag{Name: "sessions"}).WithDescription(
			"Agent session lifecycle and messaging"),
		*(&openapi31.Tag{Name: "prs"}).WithDescription(
			"Pull-request actions (SCM lane)"),
		*(&openapi31.Tag{Name: "reviews"}).WithDescription(
			"Code-review runs and findings"),
		*(&openapi31.Tag{Name: "notifications"}).WithDescription(
			"Durable dashboard notifications"),
		*(&openapi31.Tag{Name: "usage"}).WithDescription(
			"Token usage telemetry for AO sessions"),
		*(&openapi31.Tag{Name: "push"}).WithDescription(
			"Mobile push-device registration for OS push notifications"),
		*(&openapi31.Tag{Name: "events"}).WithDescription(
			"Server-sent CDC event stream with durable replay"),
		*(&openapi31.Tag{Name: "import"}).WithDescription(
			"Legacy AO project import (availability probe and run)"),
		*(&openapi31.Tag{Name: "dev"}).WithDescription(
			"Developer-only maintenance operations"),
		*(&openapi31.Tag{Name: "mobile"}).WithDescription(
			"Connect Mobile LAN bridge control (loopback/desktop only)"),
		*(&openapi31.Tag{Name: "browser"}).WithDescription(
			"Target-isolated desktop browser runtime (loopback only)"),
		*(&openapi31.Tag{Name: "workflows"}).WithDescription(
			"Durable workflow runs (Checkpoint 8A structure only — no execution)"),
		*(&openapi31.Tag{Name: "auth"}).WithDescription(
			"User identity, login/logout sessions, and current-user resolution (Checkpoint 8P-A)"),
	}

	for _, op := range operations() {
		oc, err := r.NewOperationContext(op.method, op.path)
		if err != nil {
			return nil, fmt.Errorf("new operation %s %s: %w", op.method, op.path, err)
		}
		oc.SetID(op.id)
		oc.SetSummary(op.summary)
		oc.SetTags(op.tag)
		for _, param := range op.pathParams {
			oc.AddReqStructure(param)
		}
		if op.reqBody != nil {
			// AddReqStructure leaves requestBody.required absent, which
			// OpenAPI reads as optional. Most of these bodies are mandatory, so
			// force it — otherwise validators/generators treat the body as
			// skippable. Ops that genuinely accept an empty body opt out.
			if op.optionalReqBody {
				oc.AddReqStructure(op.reqBody)
			} else {
				oc.AddReqStructure(op.reqBody, openapi.WithCustomize(markRequestBodyRequired))
			}
		}
		for _, resp := range op.resps {
			opts := []openapi.ContentOption{openapi.WithHTTPStatus(resp.status)}
			if op.contentTypes != nil && op.contentTypes[resp.status] != "" {
				opts = append(opts, openapi.WithContentType(op.contentTypes[resp.status]))
			}
			oc.AddRespStructure(resp.body, opts...)
		}
		if err := r.AddOperation(oc); err != nil {
			return nil, fmt.Errorf("add operation %s %s: %w", op.method, op.path, err)
		}
	}

	return r.Spec.MarshalYAML()
}

// schemaName maps swaggest's default PackageType component names (e.g.
// "ProjectProject", "EnvelopeAPIError") to the clean, stable schema names that
// become the generated TypeScript type names. Every reflected type is listed
// explicitly: an unrecognised default name is returned verbatim, so a new type
// surfaces as a visibly-wrong "PackageType" name in the diff (and the drift
// test) rather than silently colliding with an existing schema via a
// TrimPrefix catch-all.
func schemaName(_ reflect.Type, defaultName string) string {
	if clean, ok := schemaNames[defaultName]; ok {
		return clean
	}
	return defaultName
}

// schemaNames is the exhaustive default→clean mapping for every type reflected
// by projectOperations(). Add an entry when a new contract type is introduced;
// the drift test fails until the spec is regenerated, which flags the gap.
var schemaNames = map[string]string{
	"ControllersAdminUserView":                        "AdminUserView",
	"ControllersListUsersResponse":                    "ListUsersResponse",
	"ControllersUserResponse":                         "UserResponse",
	"ControllersCreateUserRequest":                    "CreateUserRequest",
	"ControllersSetUserRoleRequest":                   "SetUserRoleRequest",
	"ControllersSetUserStatusRequest":                 "SetUserStatusRequest",
	"ControllersProjectIntelligenceOverview":          "ProjectIntelligenceOverview",
	"ControllersProjectIntelligenceRepoStatus":        "ProjectIntelligenceRepoStatus",
	"ControllersProjectIntelligenceArchitecture":      "ProjectIntelligenceArchitecture",
	"ControllersProjectIntelligenceSubgraph":          "ProjectIntelligenceSubgraph",
	"ControllersProjectIntelligenceSubgraphNode":      "ProjectIntelligenceSubgraphNode",
	"ControllersProjectIntelligenceSubgraphEdge":      "ProjectIntelligenceSubgraphEdge",
	"ControllersProjectIntelligenceSearchResult":      "ProjectIntelligenceSearchResult",
	"ControllersProjectIntelligenceSearchHit":         "ProjectIntelligenceSearchHit",
	"ControllersProjectIntelligenceContextPreview":    "ProjectIntelligenceContextPreview",
	"ControllersProjectIntelligenceContextSection":    "ProjectIntelligenceContextSection",
	"ControllersProjectIntelligenceContextItem":       "ProjectIntelligenceContextItem",
	"ControllersProjectIntelligenceContextGraph":      "ProjectIntelligenceContextGraph",
	"ControllersProjectIntelligenceSyncResponse":      "ProjectIntelligenceSyncResponse",
	"ControllersWorkItemsConfigResponse":              "WorkItemsConfigResponse",
	"ControllersWorkItemsConfigUpdate":                "WorkItemsConfigUpdate",
	"ControllersWorkItemsConnectionResponse":          "WorkItemsConnectionResponse",
	"ControllersWorkItemsProviderProject":             "WorkItemsProviderProject",
	"ControllersWorkItemsProviderProjectsResponse":    "WorkItemsProviderProjectsResponse",
	"ControllersWorkItemLinkRequest":                  "WorkItemLinkRequest",
	"ControllersWorkItemLinkResponse":                 "WorkItemLinkResponse",
	"ControllersWorkItemLinksResponse":                "WorkItemLinksResponse",
	"ControllersWorkItemLinkSyncRequest":              "WorkItemLinkSyncRequest",
	"ControllersWorkItemsHealthResponse":              "WorkItemsHealthResponse",
	"ControllersWorkItemsSyncResponse":                "WorkItemsSyncResponse",
	"ControllersWorkItemsAuditEntry":                  "WorkItemsAuditEntry",
	"ControllersWorkItemsAuditResponse":               "WorkItemsAuditResponse",
	"ControllersProjectIntelligenceMemorySync":        "ProjectIntelligenceMemorySync",
	"ControllersTenantView":                           "TenantView",
	"ControllersTenantMemberView":                     "TenantMemberView",
	"ControllersListTenantsResponse":                  "ListTenantsResponse",
	"ControllersTenantResponse":                       "TenantResponse",
	"ControllersListTenantMembersResponse":            "ListTenantMembersResponse",
	"ControllersCreateTenantRequest":                  "CreateTenantRequest",
	"ControllersUpdateTenantRequest":                  "UpdateTenantRequest",
	"ControllersAddTenantMemberRequest":               "AddTenantMemberRequest",
	"ControllersTeamView":                             "TeamView",
	"ControllersTeamMemberView":                       "TeamMemberView",
	"ControllersListTeamsResponse":                    "ListTeamsResponse",
	"ControllersTeamResponse":                         "TeamResponse",
	"ControllersListTeamMembersResponse":              "ListTeamMembersResponse",
	"ControllersCreateTeamRequest":                    "CreateTeamRequest",
	"ControllersUpdateTeamRequest":                    "UpdateTeamRequest",
	"ControllersAddTeamMemberRequest":                 "AddTeamMemberRequest",
	"ControllersOKResponse":                           "OKResponse",
	"ControllersProjectGrantView":                     "ProjectGrantView",
	"ControllersProjectAccessResponse":                "ProjectAccessResponse",
	"ControllersGrantProjectAccessRequest":            "GrantProjectAccessRequest",
	"ControllersSettingsResponse":                     "SettingsResponse",
	"ControllersUpdateSessionInterfaceRequest":        "UpdateSessionInterfaceRequest",
	"ControllersConversationSnapshotResponse":         "ConversationSnapshotResponse",
	"ControllersConversationTurnResponse":             "ConversationTurnResponse",
	"ControllersConversationTurnDiffResponse":         "ConversationTurnDiffResponse",
	"ControllersConversationDiffFileResponse":         "ConversationDiffFileResponse",
	"ControllersConversationMessageResponse":          "ConversationMessageResponse",
	"ControllersConversationActivityResponse":         "ConversationActivityResponse",
	"ControllersSendConversationMessageRequest":       "SendConversationMessageRequest",
	"ControllersConversationImageContentRequest":      "ConversationImageContentRequest",
	"ControllersConversationResourceContentRequest":   "ConversationResourceContentRequest",
	"ControllersSendConversationMessageResponse":      "SendConversationMessageResponse",
	"ControllersEditConversationMessageRequest":       "EditConversationMessageRequest",
	"ControllersConversationContentSummaryResponse":   "ConversationContentSummaryResponse",
	"ControllersEditConversationMessageResponse":      "EditConversationMessageResponse",
	"ControllersActivateConversationBranchResponse":   "ActivateConversationBranchResponse",
	"ControllersConversationBranchPointResponse":      "ConversationBranchPointResponse",
	"ControllersResolveConversationApprovalRequest":   "ResolveConversationApprovalRequest",
	"ControllersResolveConversationInputRequest":      "ResolveConversationInputRequest",
	"ControllersConversationModelsResponse":           "ConversationModelsResponse",
	"ControllersConversationModelResponse":            "ConversationModelResponse",
	"ControllersConversationConfigOptionsResponse":    "ConversationConfigOptionsResponse",
	"ControllersConversationConfigOptionResponse":     "ConversationConfigOptionResponse",
	"ControllersConversationConfigChoiceResponse":     "ConversationConfigChoiceResponse",
	"ControllersSetConversationConfigOptionRequest":   "SetConversationConfigOptionRequest",
	"ControllersConversationSkillsResponse":           "ConversationSkillsResponse",
	"ControllersConversationSkillResponse":            "ConversationSkillResponse",
	"ControllersConversationTurnSettingsPayload":      "ConversationTurnSettingsPayload",
	"ControllersConversationUsagePayload":             "ConversationUsagePayload",
	"ControllersConversationRateLimitsPayload":        "ConversationRateLimitsPayload",
	"ControllersConversationPlanResponse":             "ConversationPlanResponse",
	"ControllersConversationPlanStepResponse":         "ConversationPlanStepResponse",
	"ControllersConversationModelReroutePayload":      "ConversationModelReroutePayload",
	"ControllersConversationAccountPayload":           "ConversationAccountPayload",
	"ControllersConversationThreadStatePayload":       "ConversationThreadStatePayload",
	"ControllersConversationMCPServerPayload":         "ConversationMCPServerPayload",
	"ControllersReloadConversationMCPServersResponse": "ReloadConversationMCPServersResponse",
	"ControllersCompactConversationResponse":          "CompactConversationResponse",
	"ControllersRollbackConversationResponse":         "RollbackConversationResponse",
	"ControllersSetConversationTitleRequest":          "SetConversationTitleRequest",
	"ControllersSetConversationTitleResponse":         "SetConversationTitleResponse",
	"ControllersSteerConversationRequest":             "SteerConversationRequest",
	"ControllersSteerConversationResponse":            "SteerConversationResponse",
	"ControllersPromoteQueuedTurnResponse":            "PromoteQueuedTurnResponse",
	// httpd/envelope
	"EnvelopeAPIError": "APIError",
	// domain
	"DomainProjectID":                 "ProjectID",
	"DomainSessionID":                 "SessionID",
	"DomainIssueID":                   "IssueID",
	"DomainSession":                   "Session",
	"DomainProjectConfig":             "ProjectConfig",
	"DomainTrackerIntakeConfig":       "TrackerIntakeConfig",
	"ControllersTriggerReviewRequest": "TriggerReviewRequest",
	"DomainContainerReapConfig":       "ContainerReapConfig",
	"DomainGitPolicy":                 "GitPolicy",
	"DomainAgentConfig":               "AgentConfig",
	"DomainRoleOverride":              "RoleOverride",
	// httpd/controllers (wire envelopes)
	"ControllersListProjectsResponse":                     "ListProjectsResponse",
	"ControllersProjectResponse":                          "ProjectResponse",
	"ControllersAgentIDParam":                             "AgentIDParam",
	"ControllersGetProjectResponse":                       "ProjectGetResponse",
	"ControllersProjectOrDegraded":                        "ProjectOrDegraded",
	"ControllersListSessionsQuery":                        "ListSessionsQuery",
	"ControllersCleanupSessionsQuery":                     "CleanupSessionsQuery",
	"ControllersListSessionsResponse":                     "ListSessionsResponse",
	"ControllersSpawnSessionRequest":                      "SpawnSessionRequest",
	"ControllersSpawnSessionResponse":                     "SpawnSessionResponse",
	"ControllersSessionResponse":                          "SessionResponse",
	"ControllersSessionPreviewResponse":                   "SessionPreviewResponse",
	"ControllersSetSessionPreviewRequest":                 "SetSessionPreviewRequest",
	"ControllersStartPreviewServerRequest":                "StartPreviewServerRequest",
	"ControllersPreviewServerStatusResponse":              "PreviewServerStatusResponse",
	"ControllersBrowserStatusQuery":                       "BrowserStatusQuery",
	"ControllersBrowserStatusResponse":                    "BrowserStatusResponse",
	"ControllersBrowserCommandRequest":                    "BrowserCommandRequest",
	"ControllersBrowserCommandResponse":                   "BrowserCommandResponse",
	"ControllersSetSessionMergePolicyRequest":             "SetSessionMergePolicyRequest",
	"ControllersSetSessionMergePolicyResponse":            "SetSessionMergePolicyResponse",
	"ControllersSetSessionAutoInjectReviewRequest":        "SetSessionAutoInjectReviewRequest",
	"ControllersSetSessionAutoInjectReviewResponse":       "SetSessionAutoInjectReviewResponse",
	"ControllersSetSessionAutoInjectCIRequest":            "SetSessionAutoInjectCIRequest",
	"ControllersSetSessionAutoInjectCIResponse":           "SetSessionAutoInjectCIResponse",
	"ControllersRenameSessionRequest":                     "RenameSessionRequest",
	"ControllersSetSessionReviewerRequest":                "SetSessionReviewerRequest",
	"ControllersRenameSessionResponse":                    "RenameSessionResponse",
	"ControllersRestoreSessionResponse":                   "RestoreSessionResponse",
	"ControllersResumeAgentResponse":                      "ResumeAgentResponse",
	"ControllersSwitchAgentRequest":                       "SwitchAgentRequest",
	"ControllersAgentSwitchView":                          "AgentSwitch",
	"ControllersAgentSwitchResponse":                      "AgentSwitchResponse",
	"ControllersListAgentSwitchesResponse":                "ListAgentSwitchesResponse",
	"ControllersSubmitAgentHandoffRequest":                "SubmitAgentHandoffRequest",
	"ControllersStartSessionInterfaceTransitionRequest":   "StartSessionInterfaceTransitionRequest",
	"ControllersSessionInterfaceTransitionView":           "SessionInterfaceTransition",
	"ControllersSessionInterfaceTransitionStatusResponse": "SessionInterfaceTransitionStatusResponse",
	"ControllersStartSessionInterfaceTransitionResponse":  "StartSessionInterfaceTransitionResponse",
	"ControllersCancelSessionInterfaceTransitionResponse": "CancelSessionInterfaceTransitionResponse",
	"ControllersCleanupSessionsResponse":                  "CleanupSessionsResponse",
	"ControllersCleanupSkippedSession":                    "CleanupSkippedSession",
	"ControllersWorkspaceFileQuery":                       "WorkspaceFileQuery",
	"ControllersStageSessionAttachmentsRequest":           "StageSessionAttachmentsRequest",
	"ControllersStageSessionAttachmentsResponse":          "StageSessionAttachmentsResponse",
	"ControllersAttachmentInput":                          "AttachmentInput",
	"ControllersListWorkspaceFilesResponse":               "ListWorkspaceFilesResponse",
	"ControllersWorkspaceFileSummary":                     "WorkspaceFileSummary",
	"ControllersWorkspaceFileResponse":                    "WorkspaceFileResponse",
	"ControllersKillSessionResponse":                      "KillSessionResponse",
	"ControllersRollbackSessionResponse":                  "RollbackSessionResponse",
	"ControllersSendSessionMessageRequest":                "SendSessionMessageRequest",
	"ControllersSendSessionMessageResponse":               "SendSessionMessageResponse",
	"ControllersDelegateTaskRequest":                      "DelegateTaskRequest",
	"ControllersDelegateTaskResponse":                     "DelegateTaskResponse",
	"ControllersClaimPRResponse":                          "ClaimPRResponse",
	"ControllersClaimPRRequest":                           "ClaimPRRequest",
	"ControllersSessionPRFacts":                           "SessionPRFacts",
	"ControllersSessionPRSummary":                         "SessionPRSummary",
	"ControllersSessionPRCISummary":                       "SessionPRCISummary",
	"ControllersSessionPRFailingCheck":                    "SessionPRFailingCheck",
	"ControllersSessionPRReviewSummary":                   "SessionPRReviewSummary",
	"ControllersSessionPRReviewEntry":                     "SessionPRReviewEntry",
	"ControllersSessionPRUnresolvedReviewer":              "SessionPRUnresolvedReviewer",
	"ControllersSessionPRReviewCommentLink":               "SessionPRReviewCommentLink",
	"ControllersSessionPRMergeabilitySummary":             "SessionPRMergeabilitySummary",
	"ControllersSessionPRConflictFile":                    "SessionPRConflictFile",
	"ControllersListSessionPRsResponse":                   "ListSessionPRsResponse",
	"ControllersSetActivityRequest":                       "SetActivityRequest",
	"ControllersSetActivityResponse":                      "SetActivityResponse",
	"ControllersSetReviewActivityRequest":                 "SetReviewActivityRequest",
	"ControllersSetReviewActivityResponse":                "SetReviewActivityResponse",
	"ControllersSpawnOrchestratorRequest":                 "SpawnOrchestratorRequest",
	"ControllersSpawnOrchestratorResponse":                "SpawnOrchestratorResponse",
	"ControllersOrchestratorResponse":                     "OrchestratorResponse",
	"AgentInventory":                                      "ListAgentsResponse",
	"AgentInfo":                                           "AgentInfo",
	"AgentProbeResult":                                    "ProbeAgentResponse",
	"PortsAgentModelCatalog":                              "AgentModelsResponse",
	"PortsAgentModelInfo":                                 "AgentModelInfo",
	"ControllersListNotificationsQuery":                   "ListNotificationsQuery",
	"ControllersNotificationStreamQuery":                  "NotificationStreamQuery",
	"ControllersNotificationIDParam":                      "NotificationIDParam",
	"ControllersNotificationTarget":                       "NotificationTarget",
	"ControllersNotificationResponse":                     "NotificationResponse",
	"ControllersListNotificationsResponse":                "ListNotificationsResponse",
	"ControllersMarkNotificationReadRequest":              "MarkNotificationReadRequest",
	"ControllersNotificationEnvelope":                     "NotificationEnvelope",
	"ControllersMarkAllNotificationsReadRequest":          "MarkAllNotificationsReadRequest",
	"ControllersMarkAllNotificationsReadResponse":         "MarkAllNotificationsReadResponse",
	"ControllersUsageHookMetadata":                        "UsageHookMetadata",
	"ControllersListUsageSessionsQuery":                   "ListUsageSessionsQuery",
	"ControllersCompactSessionUsageResponse":              "CompactSessionUsageResponse",
	"ControllersListCompactSessionUsageResponse":          "ListCompactSessionUsageResponse",
	"ControllersUsageTotalsResponse":                      "UsageTotalsResponse",
	"ControllersUsageModelResponse":                       "UsageModelResponse",
	"ControllersUsageHarnessResponse":                     "UsageHarnessResponse",
	"ControllersSessionUsageResponse":                     "SessionUsageResponse",
	// httpd/controllers — standalone shell terminal wire envelopes
	"ControllersShellTerminalHandleIDParam": "ShellTerminalHandleIDParam",
	"ControllersOpenShellTerminalRequest":   "OpenShellTerminalRequest",
	"ControllersUpdateShellTerminalRequest": "UpdateShellTerminalRequest",
	"ControllersShellTerminalResponse":      "ShellTerminalResponse",
	"ControllersListShellTerminalsResponse": "ListShellTerminalsResponse",
	"ControllersShellTerminalEnvelope":      "ShellTerminalEnvelope",
	// httpd/controllers — PR wire envelopes
	"ControllersMergePRRequest":          "MergePRRequest",
	"ControllersMergePRResponse":         "MergePRResponse",
	"ControllersResolveCommentsRequest":  "ResolveCommentsRequest",
	"ControllersResolveCommentsResponse": "ResolveCommentsResponse",
	// httpd/controllers — review wire envelopes
	"ControllersListReviewsResponse":   "ListReviewsResponse",
	"ControllersReviewRunResponse":     "ReviewRunResponse",
	"ControllersTriggerReviewResponse": "TriggerReviewResponse",
	"ControllersCancelReviewResponse":  "CancelReviewResponse",
	"ControllersKillReviewResponse":    "KillReviewResponse",
	"ControllersRestoreReviewResponse": "RestoreReviewResponse",
	"ControllersSubmitReviewItem":      "SubmitReviewItem",
	"ControllersSubmitReviewInput":     "SubmitReviewInput",
	// httpd/controllers — workflow wire envelopes
	"ControllersCreateWorkflowRunRequest":         "CreateWorkflowRunRequest",
	"ControllersWorkflowAttemptView":              "WorkflowAttemptView",
	"ControllersWorkflowStepView":                 "WorkflowStepView",
	"ControllersWorkflowVerificationPlan":         "WorkflowVerificationPlan",
	"ControllersWorkflowVerificationCommand":      "WorkflowVerificationCommand",
	"ControllersWorkflowVerificationFile":         "WorkflowVerificationFile",
	"WorkflowVerifyResult":                        "WorkflowVerifyResult",
	"WorkflowVerifyCheckResult":                   "WorkflowVerifyCheckResult",
	"ControllersWorkflowRunView":                  "WorkflowRunView",
	"ControllersWorkflowStrategySignals":          "WorkflowStrategySignals",
	"ControllersWorkflowExecutionStrategyView":    "WorkflowExecutionStrategyView",
	"ControllersWorkflowRecoveryView":             "WorkflowRecoveryView",
	"ControllersWorkflowResumeView":               "WorkflowResumeView",
	"ControllersWorkflowPlanReuseView":            "WorkflowPlanReuseView",
	"ControllersWorkflowRepairPlanView":           "WorkflowRepairPlanView",
	"ControllersWorkflowRepairIntentView":         "WorkflowRepairIntentView",
	"ControllersWorkflowRepairArtifactView":       "WorkflowRepairArtifactView",
	"ControllersWorkflowRepairResponse":           "WorkflowRepairResponse",
	"ControllersWorkflowRecoveryResponse":         "WorkflowRecoveryResponse",
	"ControllersPlacementOverrideRequestBody":     "PlacementOverrideRequestBody",
	"ControllersPlacementOverrideView":            "PlacementOverrideView",
	"ControllersPlacementOverrideResponse":        "PlacementOverrideResponse",
	"ControllersPlacementTransitionRequestBody":   "PlacementTransitionRequestBody",
	"ControllersPlacementQuiescenceView":          "PlacementQuiescenceView",
	"ControllersPlacementTransitionView":          "PlacementTransitionView",
	"ControllersPlacementTransitionResponse":      "PlacementTransitionResponse",
	"ControllersCapacityUsageView":                "CapacityUsageView",
	"ControllersCapacityClaimView":                "CapacityClaimView",
	"ControllersSchedulerStatusResponse":          "SchedulerStatusResponse",
	"ControllersRuntimeGCRequest":                 "RuntimeGCRequest",
	"ControllersRuntimeGCFindingView":             "RuntimeGCFindingView",
	"ControllersRuntimeGCReportResponse":          "RuntimeGCReportResponse",
	"ControllersWorkflowBranchWaitView":           "WorkflowBranchWaitView",
	"ControllersWorkflowPendingChangesResponse":   "WorkflowPendingChangesResponse",
	"ControllersWorkflowPendingChangeView":        "WorkflowPendingChangeView",
	"ControllersCommitPendingChangesRequest":      "CommitPendingChangesRequest",
	"ControllersCommitPendingChangesResponse":     "CommitPendingChangesResponse",
	"ControllersWorkflowPresentationView":         "WorkflowPresentationView",
	"ControllersWorkflowPresentationAction":       "WorkflowPresentationAction",
	"ControllersWorkflowPresentationStage":        "WorkflowPresentationStage",
	"ControllersWorkflowPresentationPlacement":    "WorkflowPresentationPlacement",
	"ControllersWorkflowPresentationEvent":        "WorkflowPresentationEvent",
	"ControllersWorkflowPresentationTechnical":    "WorkflowPresentationTechnical",
	"ControllersWorkflowCapacityWaitView":         "WorkflowCapacityWaitView",
	"ControllersWorkflowRepairStateView":          "WorkflowRepairStateView",
	"ControllersWorkflowCapacityWaitProviderView": "WorkflowCapacityWaitProviderView",
	"ControllersWorkflowRunDetailView":            "WorkflowRunDetailView",
	"ControllersWorkflowPlanView":                 "WorkflowPlanView",
	"ControllersWorkflowTaskView":                 "WorkflowTaskView",
	"ControllersWorkflowTaskPlannerView":          "WorkflowTaskPlannerView",
	"ControllersWorkflowTaskDowngradeView":        "WorkflowTaskDowngradeView",
	"ControllersWorkflowTaskWriteScopeView":       "WorkflowTaskWriteScopeView",
	"ControllersWorkflowTaskWorktreeView":         "WorkflowTaskWorktreeView",
	"ControllersWorkflowTaskIntegrationStateView": "WorkflowTaskIntegrationStateView",
	"ControllersWorkflowBoardResponse":            "WorkflowBoardResponse",
	"ControllersWorkflowBoardEntryView":           "WorkflowBoardEntryView",
	"ControllersWorkflowBoardTaskView":            "WorkflowBoardTaskView",
	"ControllersWorkflowBoardRepairView":          "WorkflowBoardRepairView",
	"ControllersWorkflowBoardCountsView":          "WorkflowBoardCountsView",
	"ControllersProjectBoardQuery":                "ProjectBoardQuery",
	"ControllersWorkflowStepProgressView":         "WorkflowStepProgressView",
	"WorkflowMasterPlan":                          "WorkflowMasterPlan",
	"WorkflowPlannedStep":                         "WorkflowPlannedStep",
	"WorkflowPlanValidation":                      "WorkflowPlanValidation",
	"ControllersWorkflowRunResponse":              "WorkflowRunResponse",
	"ControllersListWorkflowsResponse":            "ListWorkflowsResponse",
	"ControllersWorkflowProjectIDParam":           "WorkflowProjectIDParam",
	"ControllersWorkflowIDParam":                  "WorkflowIDParam",
	"ControllersListWorkflowsQuery":               "ListWorkflowsQuery",
	"ControllersWorkflowQuestionChoiceResponse":   "WorkflowQuestionChoiceResponse",
	"ControllersWorkflowQuestionResponse":         "WorkflowQuestionResponse",
	"ControllersListWorkflowQuestionsResponse":    "ListWorkflowQuestionsResponse",
	"ControllersWorkflowQuestionResponseBody":     "WorkflowQuestionResponseBody",
	"ControllersAnswerWorkflowQuestionRequest":    "AnswerWorkflowQuestionRequest",
	"ControllersWorkflowQuestionIDParam":          "WorkflowQuestionIDParam",
	// domain review entities
	"DomainReviewRun":     "ReviewRun",
	"ReviewPRReviewState": "PRReviewState",
	// httpd/controllers: import wire envelopes
	"ControllersImportStatusResponse": "ImportStatusResponse",
	"ControllersImportRunResponse":    "ImportRunResponse",
	// httpd/controllers: dev wire envelopes
	"ControllersDevImportProjectsRequest":  "DevImportProjectsRequest",
	"ControllersDevImportProjectsResponse": "DevImportProjectsResponse",
	// httpd/controllers: mobile wire envelopes
	"ControllersMobileStatusResponse":  "MobileStatusResponse",
	"ControllersMobileDeviceResponse":  "MobileDeviceResponse",
	"ControllersMobileDevicesResponse": "MobileDevicesResponse",
	"ControllersMuteDeviceRequest":     "MuteDeviceRequest",
	"ControllersInstallIDParam":        "InstallIDParam",
	"ControllersPushPairingIDParam":    "PushPairingIDParam",
	// devimport report
	"DevimportReport":   "DevImportProjectsReport",
	"DevimportConflict": "DevImportProjectsConflict",
	// httpd/controllers: push-device wire envelopes
	"ControllersRegisterPushDeviceRequest":    "RegisterPushDeviceRequest",
	"ControllersPushDeviceEnvelope":           "PushDeviceEnvelope",
	"ControllersPushDeviceResponse":           "PushDeviceResponse",
	"ControllersUnregisterPushDeviceResponse": "UnregisterPushDeviceResponse",
	// legacyimport report
	"LegacyimportReport": "ImportReport",
	// service/project entities + DTOs
	"ProjectProject":                    "Project",
	"ProjectSummary":                    "ProjectSummary",
	"ProjectDegraded":                   "DegradedProject",
	"ProjectAddInput":                   "AddProjectInput",
	"ProjectInitializeRepositoryInput":  "InitializeRepositoryInput",
	"ProjectInitializeRepositoryResult": "InitializeRepositoryResult",
	"ProjectRemoveResult":               "RemoveProjectResult",
	"ProjectSetConfigInput":             "SetProjectConfigInput",
	"ProjectUpdateSettingsInput":        "UpdateProjectSettingsInput",
	"ProjectWorkspaceRepo":              "WorkspaceRepo",
	"SessionWorkspaceFileStatus":        "WorkspaceFileStatus",
}

// markRequestBodyRequired sets requestBody.required: true on the operation's
// JSON body. swaggest leaves it absent (== optional) for AddReqStructure bodies.
func markRequestBodyRequired(cor openapi.ContentOrReference) {
	if rb, ok := cor.(*openapi31.RequestBodyOrReference); ok && rb.RequestBody != nil {
		rb.RequestBody.WithRequired(true)
	}
}

// nonNullableSlices drops the "null" that swaggest unions into every Go slice
// type (a nil slice marshals as JSON null). A required array field should be
// `T[]`, not `T[] | null`; the handlers normalise nil to an empty slice, so
// null never reaches the wire. Byte slices (base64 strings) are left alone.
func nonNullableSlices(p jsonschema.InterceptNullabilityParams) {
	if !p.NullAdded || p.Type == nil || p.Type.Kind() != reflect.Slice {
		return
	}
	if p.Type.Elem().Kind() == reflect.Uint8 {
		return
	}
	p.Schema.TypeEns().WithSimpleTypes(jsonschema.Array)
	p.Schema.Type.SliceOfSimpleTypeValues = nil
}

// requiredFromJSONTag marks a property required when its json tag lacks
// `omitempty` (the Go convention for "always present"). Runs after default
// processing so ParentSchema exists; skips fields without a json tag (e.g. path
// params, which swaggest marks required on their own).
func requiredFromJSONTag(p jsonschema.InterceptPropParams) error {
	if !p.Processed || p.ParentSchema == nil {
		return nil
	}
	jsonTag := p.Field.Tag.Get("json")
	if jsonTag == "" || jsonTag == "-" {
		return nil
	}
	parts := strings.Split(jsonTag, ",")
	name := parts[0]
	if name == "" {
		name = p.Name
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			return nil
		}
	}
	for _, existing := range p.ParentSchema.Required {
		if existing == name {
			return nil
		}
	}
	p.ParentSchema.Required = append(p.ParentSchema.Required, name)
	return nil
}

// --- operation registry -----------------------------------------------------

type respUnit struct {
	status int
	body   any
}

type operation struct {
	method, path, id, summary string
	tag                       string
	pathParams                []any // path/query param containers (e.g. ProjectIDParam)
	reqBody                   any   // JSON request body struct, nil when the op takes none
	// optionalReqBody declares the body without marking it required, for the
	// handlers that accept an empty body as a meaningful default.
	optionalReqBody bool
	resps           []respUnit
	contentTypes    map[int]string // optional non-JSON response content types by status
}

func operations() []operation {
	ops := append([]operation{}, eventOperations()...)
	ops = append(ops, agentOperations()...)
	ops = append(ops, environmentOperations()...)
	ops = append(ops, projectOperations()...)
	ops = append(ops, sessionOperations()...)
	ops = append(ops, prOperations()...)
	ops = append(ops, reviewOperations()...)
	ops = append(ops, decisionOperations()...)
	ops = append(ops, notificationOperations()...)
	ops = append(ops, usageOperations()...)
	ops = append(ops, capacityOperations()...)
	ops = append(ops, usageSubjectOperations()...)
	ops = append(ops, projectMemoryOperations()...)
	ops = append(ops, pushOperations()...)
	ops = append(ops, importOperations()...)
	ops = append(ops, devOperations()...)
	ops = append(ops, mobileOperations()...)
	ops = append(ops, mobileDeviceOperations()...)
	ops = append(ops, browserOperations()...)
	ops = append(ops, shellTerminalOperations()...)
	ops = append(ops, workflowOperations()...)
	ops = append(ops, authOperations()...)
	ops = append(ops, providerProfileOperations()...)
	ops = append(ops, executionPolicyOperations()...)
	return ops
}

// providerProfileOperations declares the /provider-profiles and
// /providers/registry operations (Checkpoint 8P-B). Must stay 1:1 with the
// routes ProviderProfilesController.Register mounts (enforced by the parity
// test).
func providerProfileOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/providers/registry", id: "getProviderRegistry", tag: "provider-profiles",
			summary: "List every provider AO knows about, with its declared capabilities/auth methods",
			resps: []respUnit{
				{http.StatusOK, controllers.ProviderRegistryResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/provider-profiles", id: "listProviderProfiles", tag: "provider-profiles",
			summary: "List the current user's provider profiles",
			resps: []respUnit{
				{http.StatusOK, controllers.ListProviderProfilesResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/provider-profiles", id: "createProviderProfile", tag: "provider-profiles",
			summary: "Create a provider profile owned by the current user",
			reqBody: controllers.CreateProviderProfileRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.ProviderProfileResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/provider-profiles/{id}", id: "getProviderProfile", tag: "provider-profiles",
			summary:    "Get one of the current user's provider profiles",
			pathParams: []any{controllers.ProviderProfileIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProviderProfileResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/provider-profiles/{id}", id: "updateProviderProfile", tag: "provider-profiles",
			summary:    "Update a provider profile's display name, enabled state, or default model",
			pathParams: []any{controllers.ProviderProfileIDParam{}},
			reqBody:    controllers.UpdateProviderProfileRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ProviderProfileResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/provider-profiles/{id}/connect", id: "connectProviderProfile", tag: "provider-profiles",
			summary:         "Prepare the profile owner's isolated runtime-home and refresh its cached auth state",
			pathParams:      []any{controllers.ProviderProfileIDParam{}},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.ProviderProfileResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/provider-profiles/{id}/disconnect", id: "disconnectProviderProfile", tag: "provider-profiles",
			summary:         "Clear AO's cached belief that this profile is authenticated",
			pathParams:      []any{controllers.ProviderProfileIDParam{}},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.ProviderProfileResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/provider-profiles/{id}/test", id: "testProviderProfile", tag: "provider-profiles",
			summary:         "Run a connection test against the profile owner's isolated runtime-home",
			pathParams:      []any{controllers.ProviderProfileIDParam{}},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.TestProviderProfileResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/provider-profiles/{id}/setup", id: "startProviderProfileSetup", tag: "provider-profiles",
			summary:         "Start a guided setup terminal running the provider's own login flow inside the owner's isolated runtime-home (Checkpoint 8P-E.8.4)",
			pathParams:      []any{controllers.ProviderProfileIDParam{}},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.StartProviderSetupResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/provider-profiles/{id}/setup", id: "stopProviderProfileSetup", tag: "provider-profiles",
			summary:    "Stop a profile's live guided setup terminal, if any",
			pathParams: []any{controllers.ProviderProfileIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// executionPolicyOperations declares the /execution-policy operations
// (Checkpoint 8P-C). Must stay 1:1 with the routes
// ExecutionPolicyController.Register mounts (enforced by the parity test).
func executionPolicyOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/execution-policy", id: "getExecutionPolicy", tag: "execution-policy",
			summary: "Get the current user's execution/routing policy (or the bootstrap default if none is stored)",
			resps: []respUnit{
				{http.StatusOK, controllers.ExecutionPolicyResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/execution-policy", id: "putExecutionPolicy", tag: "execution-policy",
			summary: "Replace the current user's execution/routing policy",
			reqBody: controllers.PutExecutionPolicyRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ExecutionPolicyResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// authOperations declares the /auth operations (Checkpoint 8P-A). Must stay
// 1:1 with the routes AuthController.Register mounts (enforced by the
// parity test).
func authOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/auth/login", id: "login", tag: "auth",
			summary: "Authenticate with username/email + password and receive a session cookie",
			reqBody: controllers.LoginRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.LoginResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/auth/logout", id: "logout", tag: "auth",
			summary:         "Revoke the current session and clear the session cookie",
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.LogoutResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/auth/me", id: "getCurrentUser", tag: "auth",
			summary: "Resolve the current identity: a real session, trusted-local mode's synthesized admin, or no_user",
			resps: []respUnit{
				{http.StatusOK, controllers.MeResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/auth/setup-status", id: "getAuthSetupStatus", tag: "auth",
			summary: "Report whether the installation has zero users yet (first-run signup should be offered)",
			resps: []respUnit{
				{http.StatusOK, controllers.SetupStatusResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/auth/register", id: "registerFirstUser", tag: "auth",
			summary: "Create the installation's first (owner) account and sign in — rejected once an owner already exists",
			reqBody: controllers.RegisterRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.LoginResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		// P4-B: users, teams and project access. The three administration
		// surfaces the authorization model needs to be operable at all.
		{
			method: http.MethodGet, path: "/api/v1/users", id: "listUsers", tag: "users",
			summary: "P4-B: list the installation's accounts. Requires users.read.",
			resps: []respUnit{
				{http.StatusOK, controllers.ListUsersResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/users", id: "createUser", tag: "users",
			summary: "P4-B: create a local (password) account with a non-owner role. Requires users.manage.",
			reqBody: controllers.CreateUserRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.UserResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/users/{id}", id: "getUser", tag: "users",
			summary:    "P4-B: read one account. Requires users.read.",
			pathParams: []any{controllers.UserIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.UserResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/users/{id}/role", id: "setUserRole", tag: "users",
			summary:    "P4-B: change an account's installation role. The owner cannot be demoted; promoting another account to owner transfers ownership and is reserved to the owner.",
			pathParams: []any{controllers.UserIDParam{}},
			reqBody:    controllers.SetUserRoleRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.UserResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/users/{id}/status", id: "setUserStatus", tag: "users",
			summary:    "P4-B: enable or disable an account. The owner can never be disabled, and no account can disable itself.",
			pathParams: []any{controllers.UserIDParam{}},
			reqBody:    controllers.SetUserStatusRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.UserResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/users/{id}/teams", id: "listUserTeams", tag: "users",
			summary:    "P4-B: list the teams one account belongs to. Requires users.read.",
			pathParams: []any{controllers.UserIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListTeamMembersResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/teams", id: "listTeams", tag: "teams",
			summary: "P4-B: list teams. Requires teams.read.",
			resps: []respUnit{
				{http.StatusOK, controllers.ListTeamsResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/teams", id: "createTeam", tag: "teams",
			summary: "P4-B: create a team. Requires teams.manage.",
			reqBody: controllers.CreateTeamRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.TeamResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/teams/{id}", id: "getTeam", tag: "teams",
			summary:    "P4-B: read one team. Requires teams.read.",
			pathParams: []any{controllers.TeamIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.TeamResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/teams/{id}", id: "updateTeam", tag: "teams",
			summary:    "P4-B: rename, re-describe, archive or reactivate a team. Requires teams.manage.",
			pathParams: []any{controllers.TeamIDParam{}},
			reqBody:    controllers.UpdateTeamRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.TeamResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/teams/{id}", id: "deleteTeam", tag: "teams",
			summary:    "P4-B: delete a team, its memberships, and every project grant made to it. Requires teams.manage.",
			pathParams: []any{controllers.TeamIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.OKResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/teams/{id}/members", id: "listTeamMembers", tag: "teams",
			summary:    "P4-B: list a team's members. Requires teams.read.",
			pathParams: []any{controllers.TeamIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListTeamMembersResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/teams/{id}/members", id: "addTeamMember", tag: "teams",
			summary:    "P4-B: add an account to a team, or change its role in it. Requires teams.manage.",
			pathParams: []any{controllers.TeamIDParam{}},
			reqBody:    controllers.AddTeamMemberRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ListTeamMembersResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/teams/{id}/members/{userId}", id: "removeTeamMember", tag: "teams",
			summary:    "P4-B: remove an account from a team. Access inherited through the team stops on the member's next request.",
			pathParams: []any{controllers.TeamMemberParams{}},
			resps: []respUnit{
				{http.StatusOK, controllers.OKResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		// P4-C: organizations (tenants). Unlike /users and /teams these are
		// NOT gated by the global middleware -- every route is decided against
		// one organization, which the middleware cannot resolve from the path.
		{
			method: http.MethodGet, path: "/api/v1/tenants", id: "listTenants", tag: "tenants",
			summary: "P4-C: list the organizations the caller can see, each with the caller's own role in it.",
			resps: []respUnit{
				{http.StatusOK, controllers.ListTenantsResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/tenants", id: "createTenant", tag: "tenants",
			summary: "P4-C: found an organization, with the caller as its owner. Requires tenant.create.",
			reqBody: controllers.CreateTenantRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.TenantResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/tenants/{id}", id: "getTenant", tag: "tenants",
			summary:    "P4-C: read one organization. Requires tenant.read on it; an organization the caller cannot see reports 404.",
			pathParams: []any{controllers.TenantIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.TenantResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/tenants/{id}", id: "updateTenant", tag: "tenants",
			summary:    "P4-C: rename, re-describe, archive or reactivate an organization. Requires tenant.manage on it.",
			pathParams: []any{controllers.TenantIDParam{}},
			reqBody:    controllers.UpdateTenantRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.TenantResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/tenants/{id}/members", id: "listTenantMembers", tag: "tenants",
			summary:    "P4-C: list an organization's members. Requires tenant.members.read on it.",
			pathParams: []any{controllers.TenantIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListTenantMembersResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/tenants/{id}/members", id: "addTenantMember", tag: "tenants",
			summary:    "P4-C: add an account to an organization, or change its role there. Requires tenant.members.manage on it.",
			pathParams: []any{controllers.TenantIDParam{}},
			reqBody:    controllers.AddTenantMemberRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.TenantMemberView{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/tenants/{id}/members/{userId}", id: "removeTenantMember", tag: "tenants",
			summary:    "P4-C: remove an account from an organization. Everything it reached through the membership stops on its next request.",
			pathParams: []any{controllers.TenantMemberParams{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/access", id: "getProjectAccess", tag: "projects",
			summary:    "P4-B: who can reach this project, and what the caller may do in it. Requires project.access.read on the project.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectAccessResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/projects/{id}/access", id: "grantProjectAccess", tag: "projects",
			summary:    "P4-B: grant a user or a team a role on this project, or change an existing grant. Requires project.access.manage.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.GrantProjectAccessRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectAccessResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/projects/{id}/access/{subjectKind}/{subjectId}", id: "revokeProjectAccess", tag: "projects",
			summary:    "P4-B: revoke one subject's grant on this project. Requires project.access.manage.",
			pathParams: []any{controllers.ProjectGrantSubjectParams{}},
			resps: []respUnit{
				{http.StatusOK, controllers.OKResponse{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/auth/providers", id: "getAuthProviders", tag: "auth",
			summary: "P4-A: report which sign-in methods this installation offers. Public by necessity — the sign-in screen renders from it — and it carries no issuer, client id, client secret, scope or constraint.",
			resps: []respUnit{
				{http.StatusOK, controllers.AuthProvidersResponse{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/auth/oidc/start", id: "startOIDCLogin", tag: "auth",
			summary:         "P4-A: begin an OIDC Authorization Code + PKCE login and return the provider authorization URL",
			reqBody:         controllers.OIDCStartRequest{},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.OIDCStartResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/auth/oidc/callback", id: "completeOIDCLogin", tag: "auth",
			summary: "P4-A: the identity provider's redirect target. A browser flow receives the session cookie and a 302 to a bounded in-app path; a desktop flow receives a terminal HTML page and mints its session at /auth/oidc/claim instead. Never renders a token, an authorization code, or the provider's own message.",
			resps: []respUnit{
				{http.StatusFound, ""},
				{http.StatusBadRequest, ""},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
			contentTypes: map[int]string{http.StatusFound: "text/html", http.StatusBadRequest: "text/html"},
		},
		{
			method: http.MethodPost, path: "/api/v1/auth/oidc/claim", id: "claimOIDCLogin", tag: "auth",
			summary: "P4-A: redeem a finished desktop login with the handoff secret the supervisor kept on loopback. Answers pending until the person finishes at the provider; on completion the AO session arrives as a Set-Cookie header and never in the body.",
			reqBody: controllers.OIDCClaimRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.OIDCClaimResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnauthorized, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/auth/admin/reset-password", id: "adminResetPassword", tag: "auth",
			summary: "Loopback-only local recovery: reset a known account's password without a session",
			reqBody: controllers.AdminResetPasswordRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.AdminResetPasswordResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// workflowOperations declares the /workflows operations (Checkpoint 8A). Must
// stay 1:1 with the routes WorkflowsController.Register mounts (enforced by
// the parity test).
func workflowOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/projects/{projectId}/workflows", id: "createWorkflowRun", tag: "workflows",
			summary:    "Create a workflow run and seed its initial steps",
			pathParams: []any{controllers.WorkflowProjectIDParam{}},
			reqBody:    controllers.CreateWorkflowRunRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.WorkflowRunResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{projectId}/board", id: "getProjectBoard", tag: "workflows",
			summary:    "Project Board: every top-level workflow run projected onto the lifecycle vocabulary",
			pathParams: []any{controllers.ProjectBoardParam{}, controllers.ProjectBoardQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowBoardResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}", id: "getWorkflowRun", tag: "workflows",
			summary:    "Get a workflow run with its steps and attempts",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows", id: "listWorkflowRuns", tag: "workflows",
			summary:    "List workflow run summaries",
			pathParams: []any{controllers.ListWorkflowsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListWorkflowsResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/cancel", id: "cancelWorkflowRun", tag: "workflows",
			summary:    "Cancel a workflow run and its non-terminal steps",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/cancel-archive", id: "cancelAndArchiveWorkflowRun", tag: "workflows",
			summary:    "Cancel a workflow run, cascade to its child runs, and archive it off the active Board (deletes nothing)",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{projectId}/board/history", id: "getProjectBoardHistory", tag: "workflows",
			summary:    "Archived workflows for a project, newest archive first",
			pathParams: []any{controllers.ProjectBoardParam{}, controllers.ProjectBoardQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowBoardResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/start", id: "startWorkflowRun", tag: "workflows",
			summary:    "Start a pending workflow run: run its plan step and dispatch its work step's Codex worker",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/continue", id: "continueWorkflowRun", tag: "workflows",
			summary:    "Continue a workflow run: dispatch its review step's real Claude reviewer once the work step has completed (Checkpoint 8C). Idempotent no-op when nothing is currently dispatchable. P3-C: the optional body carries the authority proof from a GET /advice reading; a Continue that arrives while AO is already repairing this run is refused 409 ACTION_SUPERSEDED rather than re-entering a resume path the repair owns.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			// Optional: a caller that sends no authority proof gets exactly the
			// pre-P3-C behaviour, so requiring the body would break every
			// existing client for a check that is a courtesy, not a gate.
			reqBody:         controllers.WorkflowActionAuthorityRequest{},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/runtime/capacity", id: "getRuntimeCapacity", tag: "workflows",
			summary: "P1-C: runtime capacity. The configured concurrency limits, what currently holds each slot, and the queue in scheduling order. This is the machine's capacity (how many agent runtimes may run at once), not a provider's rate limit.",
			resps: []respUnit{
				{http.StatusOK, controllers.SchedulerStatusResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/runtime/gc", id: "runRuntimeGC", tag: "workflows",
			summary: "P1-C: sweep the runtime artifacts AO left behind. Destroys only runtimes whose ownership, exact incarnation and terminality AO can prove; everything else is reported and left alone. dryRun classifies without destroying, using the identical predicates.",
			reqBody: controllers.RuntimeGCRequest{}, optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.RuntimeGCReportResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/usage", id: "getWorkflowUsage", tag: "workflows",
			summary:    "P3-E: the canonical token/cost ledger for one run — totals, the per-role and per-model breakdown, base execution vs repair, an autonomous parent's children, the roles whose spend AO cannot observe, and the frozen budget the run is measured against. Every figure carries its own provenance: `source` says whether tokens were reported by a provider or estimated by AO, and `cost.basis` says whether money was calculated (from the named rate card) or is simply unknown. `recorded: false` means no usage rows exist for the run and must be rendered as \"no usage data recorded\", never as zero. This is the ONLY total a client may present; the per-role `usage` block on the run detail is per-SESSION and several roles can share one session.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowUsageLedgerResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{projectId}/usage", id: "getProjectUsage", tag: "workflows",
			summary:    "P3-E: a project's token/cost rollup for one period (today, 7d, 30d, all). Buckets by the instant AO DISPATCHED the work — a fact AO recorded itself — which `periodBasis` states explicitly so nobody reads these windows as a provider's billing period. `averageTokensPerWorkflow` is null, not 0, when no workflow spent anything in the range.",
			pathParams: []any{controllers.WorkflowProjectIDParam{}, projectUsageQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectUsageResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/recovery", id: "getWorkflowRecovery", tag: "workflows",
			summary:    "P1-B: the deterministic recovery assessment for a run — the one recommended action, why, whether AO may take it automatically, whether the durable plan is reusable, and whether a bounded Repair Agent is available. Derived entirely from durable facts; no model is consulted.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRecoveryResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/advice", id: "getWorkflowAdvice", tag: "workflows",
			summary:    "P3-C: the deterministic answer to \"what do I do now\" — the category (no action required / auto-recoverable / wait-only / human action / terminal), whether a person is actually needed, what AO is doing about it by itself, the offered and refused actions with the reason behind every refusal, and the authority proof a later click is revalidated against. A strict read: it writes nothing.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowAdviceResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/pending-changes", id: "getWorkflowPendingChanges", tag: "workflows",
			summary:    "P3-A: what is uncommitted in the repository this run works in, and a proposed commit message. Read-only: it runs git status and nothing else. `available: false` means AO could not read the repository, which is UNKNOWN and never \"clean\".",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowPendingChangesResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/pending-changes/commit", id: "commitWorkflowPendingChanges", tag: "workflows",
			summary:    "P3-A: commit the repository's pending work under a message the caller supplied, re-probe, and resume the run only once the tree is provably clean. There is no stash and no silent commit: a message is required, and a commit that leaves the tree dirty does not resume the run.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			reqBody:    controllers.CommitPendingChangesRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.CommitPendingChangesResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/placement", id: "getWorkflowPlacement", tag: "workflows",
			summary:    "P1-D: where this run's work happens and why it has not launched — the FROZEN execution placement and its own generation, the durable provider-attempt chain with what each attempt could prove about mutation, and the single admission verdict naming which authority is withholding the launch (capacity, branch, placement, provider or dependency). Read-only, derived from durable rows; no token is exposed and nothing here can move a placement.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowPlacementResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/placement/override", id: "requestWorkflowPlacementOverride", tag: "workflows",
			summary:    "P1-E: ask for a particular execution placement for one task. This is a REQUEST, not a move: before anything is frozen it is the input the freeze uses; once a placement IS frozen it is recorded and changes nothing until a transition consumes it, and the response says which of the two happened. `auto` withdraws a standing override and defers to selection policy.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			reqBody:    controllers.PlacementOverrideRequestBody{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.PlacementOverrideResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/placement/transition", id: "transitionWorkflowPlacement", tag: "workflows",
			summary:    "P1-E: replace one frozen placement generation with another. Refused with 409 and the refusing AUTHORITY named unless every one of them has provably let go — the run is live, no authoritative provider attempt, no outstanding capacity claim, no runtime-bound claim, no held branch lock, and no outstanding integration authority. Quiescence is proved from durable rows, never inferred from the filesystem. Repeating a transition that already happened returns it rather than minting a second generation, and there is no operation here that re-points a running obligation.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			reqBody:    controllers.PlacementTransitionRequestBody{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.PlacementTransitionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/resume", id: "resumeWorkflowRun", tag: "workflows",
			summary:    "P1-B: discharge exactly the run's outstanding durable obligation, and report which one it was. Idempotent; an obligation only a person can discharge is reported rather than driven.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/plan/reuse", id: "reuseWorkflowPlan", tag: "workflows",
			summary:    "P1-B: execute this objective's existing durable plan revision as it stands. Refused unless the plan's identity and its project context both still hold — a stale plan is never run silently.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/plan/regenerate", id: "regenerateWorkflowPlan", tag: "workflows",
			summary:    "P1-B: mint a new durable plan revision for an objective whose plan cannot be reused. The superseded revision stays auditable and its tasks stop being authoritative. Bounded, and a compare-and-set on the revision the caller observed. P3-C: the optional body carries the authority proof from a GET /advice reading; a click computed against a state the run has moved past is refused 409 ACTION_SUPERSEDED.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			// Optional for the same reason /repair's is: a caller that sends no
			// proof keeps the pre-P3-C behaviour.
			reqBody:         controllers.WorkflowActionAuthorityRequest{},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/repair", id: "repairWorkflowRun", tag: "workflows",
			summary:    "P1-B: launch a bounded Repair Agent for a repairable technical stop. Refused with the full repair plan (and why) for any condition AO must not aim a code-writing agent at, for a spent repair budget, or under a disabled repair policy. P3-C: the optional body carries the authority proof from a GET /advice reading; a click computed against a state the run has moved past — or one that arrives while AO is already repairing — is refused 409 ACTION_SUPERSEDED instead of duplicated.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			// Optional: a caller that sends no authority proof gets exactly the
			// pre-P3-C behaviour, so requiring the body would break every
			// existing client for a check that is a courtesy, not a gate.
			reqBody:         controllers.WorkflowActionAuthorityRequest{},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusAccepted, controllers.WorkflowRepairResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/tasks/{taskId}/resume", id: "resumeWorkflowTask", tag: "workflows",
			summary:    "Resume a planned task parked in needs_attention after a person has resolved what parked it (migration 0130). Idempotent: a task that is not parked is returned unchanged.",
			pathParams: []any{controllers.WorkflowTaskParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/tasks/{taskId}/fresh-review-exception", id: "authorizeWorkflowTaskFreshReviewException", tag: "workflows",
			summary: "Authorize ONE additional integration fresh review for a task parked on integration_stale_review_after_rebase whose ordinary budget is spent, " +
				"with a named human approver and a reason. The global bound is unchanged; the grant is recorded against this task and this workspace state, " +
				"and a repeat request for the same state returns the existing grant rather than widening the budget again.",
			pathParams: []any{controllers.WorkflowTaskParam{}},
			reqBody:    controllers.AuthorizeFreshReviewExceptionRequest{},
			resps: []respUnit{
				{http.StatusOK, workflowcore.IntegrationFreshReviewException{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/tasks/{taskId}/criteria/amend", id: "amendWorkflowTaskCriterion", tag: "workflows",
			summary: "Amend or declare obsolete one acceptance criterion of a planned task (migration 0132), with a named human approver, a reason and checkable evidence. " +
				"Records the original text forever, applies the amendment, and re-opens an independent review — it never approves the work.",
			pathParams: []any{controllers.WorkflowTaskParam{}},
			reqBody:    controllers.AmendTaskCriterionRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.AmendTaskCriterionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/tasks/{taskId}/criteria/resume-review", id: "resumeAmendedWorkflowTaskReview", tag: "workflows",
			summary:    "Finish an acceptance-criterion amendment whose fresh independent review never opened: retire the superseded review dispatch and re-open the review. Records no second amendment; idempotent.",
			pathParams: []any{controllers.WorkflowTaskParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		// The two operator-only recovery operations. They are separate from
		// /continue because /continue is also the wake poller's entry point, and
		// each of these DISCARDS something AO deliberately refused to act on.
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/recover/review-provenance", id: "recoverWorkflowReviewProvenance", tag: "workflows",
			summary: "Recover a run stopped because AO cannot prove which commit its approved review target was read at, and could not reconstruct it from the branch's history. " +
				"Discards that unlocatable approval and asks for exactly one fresh independent review of the workspace as it stands. " +
				"It never infers or attests an approved commit and never verifies code no reviewer has read. Human-only and bounded.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/plan/reopen", id: "reopenAmbiguousWorkflowPlan", tag: "workflows",
			summary: "Reopen planning for an objective whose planner command was in flight when the daemon restarted, so AO could not prove whether it produced a plan. " +
				"Planning starts over from scratch: nothing is adopted from the discarded planner. " +
				"The request must carry the plan's observed updatedAt, and a reopen of a state that has since changed is refused. Human-only and bounded.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			reqBody:    controllers.ReopenAmbiguousPlanRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowRunResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		// Checkpoint 8P-E.18 — the Incident Advisor. Four operations, mirroring
		// the four routes: the split between proposing and executing is the
		// authorization boundary, not a REST style choice.
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/incident", id: "getWorkflowIncident", tag: "workflows",
			summary:    "Explain why a stopped workflow run is waiting for a person: the incident, its bounded evidence pack, and any diagnosis with AO's own reading of the proposed action",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.IncidentResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/incident/diagnose", id: "diagnoseWorkflowIncident", tag: "workflows",
			summary:    "Investigate a stopped run with an isolated, read-only Diagnostic Agent over a bounded evidence pack. Bounded per incident.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusAccepted, controllers.IncidentResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/incident/diagnosis", id: "submitWorkflowIncidentDiagnosis", tag: "workflows",
			summary:    "Record a Diagnostic Agent's validated classification and proposed action. This endpoint cannot execute anything.",
			pathParams: []any{controllers.WorkflowIDParam{}},
			reqBody:    controllers.IncidentDiagnosisSubmissionBody{},
			resps: []respUnit{
				{http.StatusOK, controllers.IncidentResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/incident/execute", id: "executeWorkflowIncidentAction", tag: "workflows",
			summary:         "Carry out an incident's diagnosed action, after AO's authorization policy and — for anything beyond the ordinary continue path — an explicit human approval",
			pathParams:      []any{controllers.WorkflowIDParam{}},
			reqBody:         controllers.ExecuteIncidentRequest{},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.IncidentResponse{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/plan/generate", id: "generateWorkflowPlan", tag: "workflows", summary: "Generate and deterministically validate a durable master plan", pathParams: []any{controllers.WorkflowIDParam{}}, resps: []respUnit{{http.StatusOK, controllers.WorkflowRunResponse{}}, {http.StatusConflict, envelope.APIError{}}, {http.StatusUnprocessableEntity, envelope.APIError{}}}},
		{method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/plan", id: "getWorkflowPlan", tag: "workflows", summary: "Get a durable master plan and planned tasks", pathParams: []any{controllers.WorkflowIDParam{}}, resps: []respUnit{{http.StatusOK, controllers.WorkflowRunResponse{}}, {http.StatusNotFound, envelope.APIError{}}}},
		{method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/plan/approve", id: "approveWorkflowPlan", tag: "workflows", summary: "Approve a validated plan and dispatch the first eligible task", pathParams: []any{controllers.WorkflowIDParam{}}, resps: []respUnit{{http.StatusOK, controllers.WorkflowRunResponse{}}, {http.StatusConflict, envelope.APIError{}}, {http.StatusUnprocessableEntity, envelope.APIError{}}}},
		{method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/plan/reject", id: "rejectWorkflowPlan", tag: "workflows", summary: "Reject and cancel a master plan", pathParams: []any{controllers.WorkflowIDParam{}}, resps: []respUnit{{http.StatusOK, controllers.WorkflowRunResponse{}}, {http.StatusConflict, envelope.APIError{}}}},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/questions", id: "listWorkflowQuestions", tag: "workflows",
			summary:    "List durable questions (Checkpoint 8K-A) captured for a workflow run",
			pathParams: []any{controllers.WorkflowIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListWorkflowQuestionsResponse{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/questions/pending", id: "listPendingDecisions", tag: "decisions",
			summary:    "List open/in-flight questions across all workflow runs (Checkpoint 8K-B pass 3's global Pending Decisions inbox)",
			pathParams: []any{controllers.PendingDecisionsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListWorkflowQuestionsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/workflows/{workflowId}/questions/{questionId}", id: "getWorkflowQuestion", tag: "workflows",
			summary:    "Get one durable question captured for a workflow run",
			pathParams: []any{controllers.WorkflowQuestionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowQuestionResponseBody{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/workflows/{workflowId}/questions/{questionId}/answer", id: "answerWorkflowQuestion", tag: "workflows",
			summary:    "Submit a human answer for a question that is awaiting one (Checkpoint 8K-A)",
			pathParams: []any{controllers.WorkflowQuestionIDParam{}},
			reqBody:    controllers.AnswerWorkflowQuestionRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkflowQuestionResponseBody{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

func browserOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/browser/status", id: "getBrowserStatus", tag: "browser",
			summary:    "Check whether the desktop browser runtime is connected for a session",
			pathParams: []any{controllers.BrowserStatusQuery{}, controllers.BrowserCapabilityHeader{}},
			resps: []respUnit{
				{http.StatusOK, controllers.BrowserStatusResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/browser/commands", id: "executeBrowserCommand", tag: "browser",
			summary:    "Execute a target-scoped command in a session's desktop browser",
			pathParams: []any{controllers.BrowserCapabilityHeader{}},
			reqBody:    controllers.BrowserCommandRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.BrowserCommandResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusServiceUnavailable, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// projectUsageQuery is the period selector for the P3-E project rollup.
type projectUsageQuery struct {
	Range *string `query:"range,omitempty" enum:"today,7d,30d,all" description:"Rollup period. Defaults to 7d. Buckets by dispatch time, not by any provider billing period."`
}

type conversationSnapshotQuery struct {
	BeforeSequence *int64 `query:"beforeSequence,omitempty" minimum:"1" description:"Read items older than this conversation sequence. Omit for the newest page."`
	Limit          *int64 `query:"limit,omitempty" minimum:"1" maximum:"500" description:"Maximum combined messages and activities to return. Defaults to 200."`
}

func usageSubjectOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/usage/subject-hook", id: "recordSubjectUsageHook", tag: "usage",
			summary: "P3-E: a runtime pane reports its OWN provider token spend. Used by reviewer and decision-resolver panes, which are not AO sessions and therefore cannot report through the session activity route. Deliberately usage-only: it carries no activity state and no session id, so a pane can say what it spent without being able to touch any session's lifecycle. A session subject is refused -- sessions report through /sessions/{id}/activity, which validates a launch id this route has no business touching.",
			reqBody: controllers.SubjectUsageHookRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SubjectUsageHookResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
			},
		},
	}
}

func capacityOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/capacity", id: "listCapacity", tag: "capacity",
			summary: "List Checkpoint 8J capacity/quota snapshots per known harness",
			resps: []respUnit{
				{http.StatusOK, controllers.ListCapacityResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// projectMemoryOperations registers P2-A's project-memory surface: read the
// state of a project's memory, inspect the facts in it, and the two repairs
// that follow from a bad answer.
func projectMemoryOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory", id: "getProjectMemoryStatus", tag: "projects",
			summary:    "P2-A: project memory status per repository. What generation the memory is at, which commit it was derived from, how many facts it holds and how many of them AO can still vouch for, and whether an indexing pass is running. A repository absent from this list has never been indexed.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListProjectMemoryResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/items", id: "listProjectMemoryItems", tag: "projects",
			summary:    "P2-A: inspect the stored facts of one repository's project memory, including the stale and invalidated ones — seeing those is the point of an inspect. Bodies are omitted; this answers what AO remembers and whether it can still vouch for it.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.ListProjectMemoryItemsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListProjectMemoryItemsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/report", id: "getProjectMemoryReport", tag: "projects",
			summary:    "P2-B: is this project's memory warm, and what is it costing each role. Runs the ordinary lifecycle freshness check (so a warm project costs a row read and no file I/O) and then assembles each role's pack exactly as a dispatch would, reporting the budget, the items and bytes selected, the estimated tokens, and what the budget excluded. syncKind=none means memory was already at the repository's current commit — the warm path the optimisation exists to produce.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.GetProjectMemoryReportQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectMemoryReportResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/knowledge", id: "listProjectMemoryKnowledge", tag: "projects",
			summary:    "P2-C: what tasks have taught this project, and what of it still holds. Task results, decisions and known risks, each with the lifecycle that decides whether retrieval would serve it: active, superseded (and by what), resolved (and by whom), obsolete, or conflicting. Active only by default, because that is what a task would actually receive; nothing is ever deleted, so asking for another status reconstructs what the project used to believe.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.ListProjectMemoryKnowledgeQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListProjectMemoryKnowledgeResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/manifests", id: "listProjectMemoryContextManifests", tag: "projects",
			summary:    "P2-C: what one execution was actually told. A context manifest records the identities of the memory facts a dispatch received — never the prompt, and never the facts' text, which may have been superseded since. It is what makes \"the Worker was working from a stale decision\" checkable rather than suspected. Names a task or a workflow run; expand=true resolves the ids back into the facts and reports any that no longer exist.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.ListProjectMemoryManifestsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListProjectMemoryManifestsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/memory/rebuild", id: "rebuildProjectMemory", tag: "projects",
			summary:    "P2-A: re-derive one repository's project memory. Bounded by the indexer's limits and restart-safe; purge deletes the existing facts first, which is the escape hatch for memory that is wrong in a way a re-derivation cannot fix.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.RebuildProjectMemoryRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.RebuildProjectMemoryResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/memory/invalidate", id: "invalidateProjectMemory", tag: "projects",
			summary:    "P2-A: retire project memory that can no longer be vouched for. With paths, retires exactly what those paths proved. With no paths, runs drift detection and applies what it finds — the honest repair for \"something moved and I cannot say what\". Nothing is deleted; facts are marked so they stop being served.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.InvalidateProjectMemoryRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.InvalidateProjectMemoryResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/memory/validate", id: "validateProjectMemory", tag: "projects",
			summary:    "P2-D: check which facts AO can still prove it is entitled to serve. Distinct from invalidate, which asks whether a fact's source FILES moved: this asks whether its LICENCE still holds — same repository identity, a mutation-provenance row behind every canonical promotion, a provenance kind this build can check. Dry run unless apply is set, and it only ever demotes.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.ValidateProjectMemoryRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ValidateProjectMemoryResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/memory/prune", id: "pruneProjectMemory", tag: "projects",
			summary:    "P2-E: retire canonical project memory that was built from a task's isolated worktree instead of from the repository. A worktree is a checkout of a repository AO already knows, not a second repository; indexing one produced canonical facts derived from an unintegrated branch. Dry run unless apply is set, and it refuses anything it cannot prove is safe: registered repositories, workspaces a live execution is using, and any prune that would leave the project with no memory at all.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.PruneProjectMemoryRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.PruneProjectMemoryResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/graph", id: "getProjectMemoryGraphStatus", tag: "projects",
			summary:    "The code graph's state per repository: which backend is serving it (by its real name -- the in-tree one is reported as \"local\" and never under a vendor's name), which generation and commit, how many files, symbols and relations it holds, what the last sync had to do, and the bounded architecture summary derived from it. drift is non-empty when the graph can no longer be vouched for -- the checkout moved on, or it is no longer the same repository -- and such a graph is reported unhealthy even though its rows are intact.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListProjectMemoryGraphResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/memory/graph/sync", id: "syncProjectMemoryGraph", tag: "projects",
			summary:    "Bring one repository's code graph up to its current commit. It chooses incremental or full exactly as a dispatch would, so an operator running it by hand exercises the production path: incremental whenever a change set can be proved, a full build otherwise. filesParsed against filesReused is the measurement -- an incremental sync of a one-file commit parses one file, and a full pass over an unchanged tree parses none.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.SyncProjectMemoryGraphQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectMemoryGraphSyncResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/graph/query", id: "queryProjectMemoryGraph", tag: "projects",
			summary:    "Ask the code graph what a dispatch would be told. Given a symbol, a file, or free text from an objective, it returns the bounded neighbourhood: the matching declarations with their signatures and summaries, what reaches them, what they reach, the tests proven to cover them, the routes that arrive at them and the tables they touch. consideredSymbols against the returned count is what makes the bound visible.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.GetProjectMemoryGraphQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectMemoryGraphAnswerResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		// P4-G: Project Intelligence. Every route is project-scoped and gated
		// on memory.read (or project.manage for the two that do work), so a
		// project in another organization answers 404 exactly as /projects/{id}
		// does.
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/intelligence", id: "getProjectIntelligence", tag: "projects",
			summary:    "P4-G: what AO knows about this project, per repository — the derived lifecycle state (pending, indexing, ready, stale, failed), the commit the graph describes against the one the checkout is actually at, the counts, what the last sync had to do, and how long it took. Stale is reported rather than smoothed over: serving structure AO cannot vouch for as current is the one failure this subsystem refuses to make.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceOverview{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/intelligence/architecture", id: "getProjectIntelligenceArchitecture", tag: "projects",
			summary:    "P4-G: the structural summary the build derived — modules, entry points, services, storage and the dependencies between them. Read from the row the build wrote, never recomputed: the summary belongs to a generation and cannot have changed since it.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.ProjectIntelligenceRepoQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceArchitecture{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/intelligence/graph", id: "getProjectIntelligenceGraph", tag: "projects",
			summary:    "P4-G: a bounded neighbourhood of the code graph, walked outward from a named symbol or file. There is deliberately no whole-graph export — a seed is required, depth is capped at two hops, and both nodes and edges have ceilings a caller cannot raise. A walk that hits one reports truncated rather than returning a partial answer as if it were whole.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.GetProjectIntelligenceGraphQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceSubgraph{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/intelligence/search", id: "searchProjectIntelligence", tag: "projects",
			summary:    "P4-G: ask a question of the two authorities AO already has — durable project memory and the code graph — and get one ranked answer list with provenance on every row. Every hit says which authority produced it, because a fact somebody wrote and a symbol AO parsed are different kinds of claim.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.GetProjectIntelligenceSearchQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceSearchResult{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/intelligence/context", id: "previewProjectIntelligenceContext", tag: "projects",
			summary:    "P4-G: the context pack one role would actually be handed, assembled by the same call a dispatch makes. Input observability only — it shows AO's own construction from AO's own durable rows, never anything a model produced. The measurements are named 'selected' and 'avoided' rather than 'saved': AO cannot see what the coding harness reads inside the worktree, so it cannot claim to have prevented a read.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.GetProjectIntelligenceContextQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceContextPreview{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/intelligence/sync", id: "syncProjectIntelligence", tag: "projects",
			summary:    "P4-G/P4-H: bring the graph AND durable project memory up to date now, each choosing between an incremental pass and a full build exactly as a dispatch would. Normal operation does not need this: the reconciler keeps every project current on its own, and this is the button for when somebody does not want to wait for it. Memory runs second, so its high-level facts read the graph this call just built.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.ProjectIntelligenceRepoQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceSyncResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/intelligence/rebuild", id: "rebuildProjectIntelligence", tag: "projects",
			summary:    "P4-G/P4-H: discard the served graph and build it again from scratch, then re-derive every durable memory fact. The repair for knowledge an operator has reason to distrust, and expensive on a large repository — the frontend confirms before calling it.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.ProjectIntelligenceRepoQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectIntelligenceSyncResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		// --- P4-E: external work management -----------------------------
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/workitems", id: "getProjectWorkItemsConfig", tag: "projects",
			summary:    "P4-E: this project's external work-management connection (Plane): which workspace and project it maps to, whether a credential is stored, and whether the last connection check passed. The credential itself is never returned.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsConfigResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/projects/{id}/workitems", id: "putProjectWorkItemsConfig", tag: "projects",
			summary:    "P4-E: configure the connection. Every field is optional and omitting one leaves it unchanged — in particular, omitting apiToken keeps the stored credential rather than clearing it. A configuration may be saved incomplete; only switching it on requires a workspace, a project and a token.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.WorkItemsConfigUpdate{},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsConfigResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/projects/{id}/workitems", id: "deleteProjectWorkItemsConfig", tag: "projects",
			summary:    "P4-E: disconnect the project, deleting the stored credential. Links are left alone: forgetting how to reach a provider is not a reason to forget which items the work was about.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/workitems/test", id: "testProjectWorkItemsConnection", tag: "projects",
			summary:    "P4-E: verify the configured credential against the provider and record the result. It lists the workspace's projects, which proves both that the token is valid and that it can see the workspace it is configured for.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsConnectionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/workitems/projects", id: "listProjectWorkItemsProviderProjects", tag: "projects",
			summary:    "P4-E: the projects in the connected workspace, so a person mapping this AO project can choose one rather than paste a UUID.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsProviderProjectsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/workitems/health", id: "getProjectWorkItemsHealth", tag: "projects",
			summary:    "P4-E: whether the integration is configured, switched on, connected or degraded, and how much is queued or permanently failed. It makes no provider call: a status endpoint that probes is slow exactly when the thing it reports on is broken.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsHealthResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/workitems/audit", id: "listProjectWorkItemsAudit", tag: "projects",
			summary:    "P4-E: the recent provider operations — what AO sent, what came back, and whether it was retried. Never a credential, a request header, or an untruncated provider body.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.WorkItemsAuditQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsAuditResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/workitems/links", id: "listProjectWorkItemLinks", tag: "projects",
			summary:    "P4-E: the external items this project's work is linked to. With live=true each is refreshed from the provider; a link the provider could not answer for comes back with its cached title and state and stale=true, rather than disappearing.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.WorkItemsLinksQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemLinksResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/workitems/links", id: "createProjectWorkItemLink", tag: "projects",
			summary:    "P4-E: link a workflow run or planned task to an external work item, either by naming one that exists (\"PROJ-123\" or a provider URL) or by asking AO to create it. AO never creates items on its own — internal repair and reviewer work does not mint anything on somebody's board.",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.WorkItemLinkRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemLinkResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/projects/{id}/workitems/links/{linkId}", id: "deleteProjectWorkItemLink", tag: "projects",
			summary:    "P4-E: unlink. The external item is left untouched — deleting somebody's planning item because a link was removed would be a destructive surprise.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.WorkItemsLinkIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/workitems/links/{linkId}/sync", id: "setProjectWorkItemLinkSync", tag: "projects",
			summary:    "P4-E: turn state and comment pushing on or off for one link. A link is useful without it — recording that two things are about each other is worth doing on its own.",
			pathParams: []any{controllers.ProjectIDParam{}, controllers.WorkItemsLinkIDParam{}},
			reqBody:    controllers.WorkItemLinkSyncRequest{},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/workitems/sync", id: "syncProjectWorkItems", tag: "projects",
			summary:    "P4-E: drain the outbound sync queue now. Normal operation does not need this — a background worker drains it on its own — and it is the button for when somebody does not want to wait for the next tick.",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkItemsSyncResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/{id}/memory/provenance/{itemId}", id: "getProjectMemoryProvenance", tag: "projects",
			summary:    "P2-D: the full evidence chain behind one fact — why it is valid, which task produced it, which commit supports it, how it became canonical, what withdrew it, and what replaced it. Retired edges are included: a superseded decision's supersedes edge is not in the current graph and is usually what is being looked for.",
			pathParams: []any{controllers.ProjectMemoryItemIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectMemoryProvenanceResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

func usageOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/usage/sessions", id: "listCompactSessionUsage", tag: "usage",
			summary:    "List compact token usage for session cards",
			pathParams: []any{controllers.ListUsageSessionsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListCompactSessionUsageResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/usage/sessions/{sessionId}", id: "getSessionUsage", tag: "usage",
			summary:    "Get detailed token usage for one session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionUsageResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// shellTerminalOperations describes the standalone shell terminal surface:
// shells the user opens by hand, with no agent session behind them.
func shellTerminalOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/settings", id: "getSettings", tag: "settings",
			summary: "Read the daemon-owned user preferences",
			resps: []respUnit{
				{http.StatusOK, controllers.SettingsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/settings/session-interface", id: "updateSessionInterface", tag: "settings",
			summary: "Choose the default interface for new sessions",
			reqBody: controllers.UpdateSessionInterfaceRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SettingsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/settings/email-notifications", id: "getEmailNotificationSettings", tag: "settings",
			summary: "Read the completion-email configuration (never the password)",
			resps: []respUnit{
				{http.StatusOK, controllers.EmailNotificationSettingsEnvelope{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/settings/email-notifications", id: "updateEmailNotificationSettings", tag: "settings",
			summary: "Change the completion-email configuration",
			reqBody: controllers.UpdateEmailNotificationSettingsRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.EmailNotificationSettingsEnvelope{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/settings/email-notifications/test", id: "sendTestEmail", tag: "settings",
			summary: "Send a test email to the configured recipient",
			resps: []respUnit{
				{http.StatusOK, controllers.SendTestEmailResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/conversation", id: "getSessionConversation", tag: "conversations",
			summary:    "Read a chat session's durable conversation",
			pathParams: []any{controllers.SessionIDParam{}, conversationSnapshotQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ConversationSnapshotResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/messages", id: "sendSessionConversationMessage", tag: "conversations",
			summary:    "Send a message to a chat session's agent",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SendConversationMessageRequest{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.SendConversationMessageResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/turns/{turnId}/edit", id: "editSessionConversationMessage", tag: "conversations",
			summary:    "Branch before and replace an earlier human prompt",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationTurnIDParam{}},
			reqBody:    controllers.EditConversationMessageRequest{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.EditConversationMessageResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/branches/{branchId}/activate", id: "activateSessionConversationBranch", tag: "conversations",
			summary:    "Resume a durable conversation branch without sending",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationBranchIDParam{}},
			resps: []respUnit{
				{http.StatusAccepted, controllers.ActivateConversationBranchResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/approvals/{requestId}/resolve", id: "resolveSessionConversationApproval", tag: "conversations",
			summary:    "Answer a pending approval in a chat session",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationRequestIDParam{}},
			reqBody:    controllers.ResolveConversationApprovalRequest{},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/inputs/{requestId}/resolve", id: "resolveSessionConversationInput", tag: "conversations",
			summary:    "Answer a structured input request in a chat session",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationRequestIDParam{}},
			reqBody:    controllers.ResolveConversationInputRequest{},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/compact", id: "compactSessionConversation", tag: "conversations",
			summary:    "Summarize earlier history to reclaim context in a chat session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusAccepted, controllers.CompactConversationResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/mcp/reload", id: "reloadSessionConversationMcpServers", tag: "conversations",
			summary:    "Restart the tool servers a chat session can reach",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ReloadConversationMCPServersResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/conversation/models", id: "listSessionConversationModels", tag: "conversations",
			summary:    "List the models the provider offers for a chat session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ConversationModelsResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/conversation/config-options", id: "listSessionConversationConfigOptions", tag: "conversations",
			summary:    "List the live session controls the provider advertises",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ConversationConfigOptionsResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/sessions/{sessionId}/conversation/config-options/{configId}", id: "setSessionConversationConfigOption", tag: "conversations",
			summary:    "Choose one provider-advertised session configuration value",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationConfigIDParam{}},
			reqBody:    controllers.SetConversationConfigOptionRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ConversationConfigOptionsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/conversation/skills", id: "listSessionConversationSkills", tag: "conversations",
			summary:    "List the named skills the provider offers for a chat session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ConversationSkillsResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/sessions/{sessionId}/conversation/settings", id: "setSessionConversationTurnSettings", tag: "conversations",
			summary:    "Choose the model, reasoning effort and approval mode for the next turn",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.ConversationTurnSettingsPayload{},
			resps: []respUnit{
				{http.StatusOK, controllers.ConversationTurnSettingsPayload{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/interrupt", id: "interruptSessionConversationTurn", tag: "conversations",
			summary:    "Cancel the in-flight turn in a chat session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/steer", id: "steerSessionConversationTurn", tag: "conversations",
			summary:    "Send guidance into the in-flight turn of a chat session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SteerConversationRequest{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.SteerConversationResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/turns/{turnId}/steer", id: "promoteQueuedSessionConversationTurn", tag: "conversations",
			summary:    "Promote a queued message into the in-flight turn",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationTurnIDParam{}},
			resps: []respUnit{
				{http.StatusAccepted, controllers.PromoteQueuedTurnResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/conversation/turns/{turnId}/rollback", id: "rollbackSessionConversation", tag: "conversations",
			summary:    "Discard a turn and everything after it from the agent's memory",
			pathParams: []any{controllers.SessionIDParam{}, controllers.ConversationTurnIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.RollbackConversationResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/sessions/{sessionId}/conversation/title", id: "setSessionConversationTitle", tag: "conversations",
			summary:    "Name the provider's conversation thread",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetConversationTitleRequest{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.SetConversationTitleResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/shell-terminals", id: "listShellTerminals", tag: "shellTerminals",
			summary: "List the standalone shell terminals owned by the current app run",
			resps: []respUnit{
				{http.StatusOK, controllers.ListShellTerminalsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/shell-terminals", id: "openShellTerminal", tag: "shellTerminals",
			summary: "Open a standalone shell terminal",
			reqBody: controllers.OpenShellTerminalRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.ShellTerminalEnvelope{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/shell-terminals/{handleId}", id: "renameShellTerminal", tag: "shellTerminals",
			summary:    "Rename a standalone shell terminal tab",
			pathParams: []any{controllers.ShellTerminalHandleIDParam{}},
			reqBody:    controllers.UpdateShellTerminalRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ShellTerminalEnvelope{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/shell-terminals/{handleId}", id: "closeShellTerminal", tag: "shellTerminals",
			summary:    "Close a standalone shell terminal and destroy its PTY",
			pathParams: []any{controllers.ShellTerminalHandleIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

func agentOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/agents", id: "listAgents", tag: "agents",
			summary: "Return cached supported and locally installed agent adapters",
			resps: []respUnit{
				{http.StatusOK, controllers.ListAgentsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/agents/refresh", id: "refreshAgents", tag: "agents",
			summary: "Refresh the cached local agent adapter catalog",
			resps: []respUnit{
				{http.StatusOK, controllers.RefreshAgentsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/agents/{agent}/probe", id: "probeAgent", tag: "agents",
			summary:    "Run a fresh local readiness probe for one agent adapter",
			pathParams: []any{controllers.AgentIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProbeAgentResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/agents/{agent}/models", id: "getAgentModels", tag: "agents",
			summary:    "Return the cached model picker for one agent, discovering it on first use",
			pathParams: []any{controllers.AgentIDParam{}, controllers.AgentModelsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.AgentModelsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/agents/{agent}/models/refresh", id: "refreshAgentModels", tag: "agents",
			summary:    "Refresh and cache the model picker for one agent",
			pathParams: []any{controllers.AgentIDParam{}, controllers.AgentModelsRefreshQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.AgentModelsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// mobileOperations declares the 5 /mobile control operations. These are
// mounted on the loopback router (mountMobile in router.go), not the REST
// /api/v1 group — only the desktop/CLI may enable, disable, or regenerate the
// phone's LAN access; the phone never toggles its own connection. Must stay
// 1:1 with the routes mountMobile registers (enforced by the parity test).
func mobileOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/mobile/status", id: "getMobileStatus", tag: "mobile",
			summary: "Check whether Connect Mobile's LAN bridge is enabled",
			resps: []respUnit{
				{http.StatusOK, controllers.MobileStatusResponse{}},
				{http.StatusForbidden, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/mobile/enable", id: "enableMobile", tag: "mobile",
			summary: "Enable the Connect Mobile LAN bridge and issue a fresh password",
			resps: []respUnit{
				{http.StatusOK, controllers.MobileStatusResponse{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/mobile/disable", id: "disableMobile", tag: "mobile",
			summary: "Disable the Connect Mobile LAN bridge",
			resps: []respUnit{
				{http.StatusOK, controllers.MobileStatusResponse{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/mobile/regenerate", id: "regenerateMobile", tag: "mobile",
			summary: "Rotate the Connect Mobile password, dropping any connected phone",
			resps: []respUnit{
				{http.StatusOK, controllers.MobileStatusResponse{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/mobile/secure-pairing", id: "setMobileSecurePairing", tag: "mobile",
			summary: "Turn TLS-over-Tailscale secure pairing on or off",
			reqBody: controllers.SetSecurePairingRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.MobileStatusResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
	}
}

// mobileDeviceOperations declares the desktop-only mobile device roster
// routes. These sit under /api/v1/mobile — like mobileOperations above — so
// they inherit the LAN listener's transport-level block; a paired phone can
// neither list nor manage the household's other devices. Must stay 1:1 with
// the routes mountMobileDevices registers (enforced by the parity test).
func mobileDeviceOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/mobile/devices", id: "listMobileDevices", tag: "mobile",
			summary: "List paired mobile devices with their live/muted status",
			resps: []respUnit{
				{http.StatusOK, controllers.MobileDevicesResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusServiceUnavailable, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/mobile/devices/{installId}", id: "muteMobileDevice", tag: "mobile",
			summary:    "Mute or unmute push notifications for a paired device",
			pathParams: []any{controllers.InstallIDParam{}},
			reqBody:    controllers.MuteDeviceRequest{},
			resps: []respUnit{
				{http.StatusOK, map[string]bool{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusServiceUnavailable, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/mobile/devices/{installId}", id: "removeMobileDevice", tag: "mobile",
			summary:    "Remove a paired device from the roster",
			pathParams: []any{controllers.InstallIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusServiceUnavailable, envelope.APIError{}},
			},
		},
	}
}

// importOperations declares the 2 /import operations. Must stay 1:1 with
// the routes ImportController.Register mounts (enforced by the parity test).
func importOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/import", id: "getImportStatus", tag: "import",
			summary: "Check whether a legacy AO install is available to import",
			resps: []respUnit{
				{http.StatusOK, controllers.ImportStatusResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/import", id: "runImport", tag: "import",
			summary: "Run the legacy AO project import through the daemon store",
			resps: []respUnit{
				{http.StatusOK, controllers.ImportRunResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// devOperations declares developer-only API operations. Must stay 1:1 with
// the routes DevController.Register mounts (enforced by the parity test).
func devOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/dev/import-projects", id: "runDevImportProjects", tag: "dev",
			summary: "Run the developer project-registry import through the daemon store",
			reqBody: controllers.DevImportProjectsRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.DevImportProjectsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

func notificationOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/notifications", id: "listNotifications", tag: "notifications",
			summary:    "List notification history",
			pathParams: []any{controllers.ListNotificationsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListNotificationsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/notifications/unread-count", id: "notificationUnreadCount", tag: "notifications",
			summary: "Unread notification count",
			resps: []respUnit{
				{http.StatusOK, controllers.NotificationUnreadCountResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/notifications/{id}", id: "markNotificationRead", tag: "notifications",
			summary:    "Mark a notification read",
			pathParams: []any{controllers.NotificationIDParam{}},
			reqBody:    controllers.MarkNotificationReadRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.NotificationEnvelope{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/notifications/read-all", id: "markAllNotificationsRead", tag: "notifications",
			summary: "Mark notifications read",
			reqBody: controllers.MarkAllNotificationsReadRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.MarkAllNotificationsReadResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/notifications/stream", id: "streamNotifications", tag: "notifications",
			summary:    "Stream created notifications",
			pathParams: []any{controllers.NotificationStreamQuery{}},
			resps: []respUnit{
				{http.StatusOK, ""},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
			contentTypes: map[int]string{http.StatusOK: "text/event-stream"},
		},
	}
}

// reviewOperations declares the session-scoped /reviews operations. Must stay
// 1:1 with the routes ReviewsController.Register mounts (enforced by the parity
// test).
// pushOperations declares the /push/devices operations. Must stay 1:1 with the
// routes PushController.Register mounts (enforced by the parity test).
func pushOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/push/devices", id: "registerPushDevice", tag: "push",
			summary: "Register (upsert) a phone's Expo push token",
			reqBody: controllers.RegisterPushDeviceRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.PushDeviceEnvelope{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/push/devices/{token}", id: "unregisterPushDevice", tag: "push",
			summary:    "Unregister a phone's Expo push token, leaving it paired",
			pathParams: []any{controllers.PushDeviceTokenParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.UnregisterPushDeviceResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/push/pairings/{id}", id: "unpairPushDevice", tag: "push",
			summary:    "Unpair this phone from the daemon, removing it from the roster",
			pathParams: []any{controllers.PushPairingIDParam{}},
			resps: []respUnit{
				{http.StatusNoContent, nil},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

func reviewOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/reviews", id: "listReviews", tag: "reviews",
			summary:    "List a worker's code-review runs",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListReviewsResponse{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/reviews/trigger", id: "triggerReview", tag: "reviews",
			summary:    "Trigger a code review of a worker's PR",
			pathParams: []any{controllers.SessionIDParam{}},
			// Optional: an empty body runs under the project's configured reviewer.
			reqBody:         controllers.TriggerReviewRequest{},
			optionalReqBody: true,
			resps: []respUnit{
				{http.StatusOK, controllers.TriggerReviewResponse{}},
				{http.StatusCreated, controllers.TriggerReviewResponse{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/reviews/cancel", id: "cancelReview", tag: "reviews",
			summary:    "Cancel a running code review",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.CancelReviewResponse{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/reviews/kill", id: "killReviewSession", tag: "reviews",
			summary:    "Kill a worker's reviewer terminal session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.KillReviewResponse{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/reviews/restore", id: "restoreReviewSession", tag: "reviews",
			summary:    "Restore a worker's reviewer terminal session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.RestoreReviewResponse{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/reviews/switch", id: "switchReviewSession", tag: "reviews",
			summary:    "Switch a worker's reviewer harness",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionReviewerRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ListReviewsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/reviews/submit", id: "submitReview", tag: "reviews",
			summary:    "Record a reviewer's result for a worker's PR",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SubmitReviewInput{},
			resps: []respUnit{
				{http.StatusOK, controllers.ReviewRunResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

type eventsQuery struct {
	After *int64 `query:"after,omitempty" minimum:"0" description:"Replay events with seq greater than this cursor. When omitted, clients may send Last-Event-ID instead."`
}

// decisionOperations declares Checkpoint 8K-B pass 2's resolver-callback
// route. Must stay 1:1 with DecisionsController.Register — TestRouteSpecParity
// fails the build otherwise.
func decisionOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/sessions/{resolverSessionId}/decisions/resolve", id: "resolveDecision", tag: "decisions",
			summary:    "Record a cross-provider Decision Resolver's result for one auto_resolvable question (Checkpoint 8K-B)",
			pathParams: []any{controllers.ResolverSessionIDParam{}},
			reqBody:    controllers.ResolveDecisionRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ResolveDecisionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

func eventOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/events", id: "streamEvents", tag: "events",
			summary:    "Stream CDC events with durable replay",
			pathParams: []any{eventsQuery{}},
			resps: []respUnit{
				{http.StatusOK, ""},
				{status: http.StatusBadRequest, body: envelope.APIError{}},
				{status: http.StatusInternalServerError, body: envelope.APIError{}},
				{status: http.StatusNotImplemented, body: envelope.APIError{}},
			},
			contentTypes: map[int]string{http.StatusOK: "text/event-stream"},
		},
	}
}

// projectOperations declares the canonical /projects operations. The set must
// stay 1:1 with the routes ProjectsController.Register mounts —
// TestRouteSpecParity fails the build otherwise.
func environmentOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/environment/status", id: "getEnvironmentStatus", tag: "environment",
			summary: "Report real local Codex/Claude/GitHub/project readiness for the Setup UX",
			resps: []respUnit{
				{http.StatusOK, controllers.EnvironmentStatusResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/environment/github/test", id: "testGitHubConnection", tag: "environment",
			summary: "Run a fresh, cheap local GitHub CLI probe (no REST call, never returns a token)",
			resps: []respUnit{
				{http.StatusOK, controllers.GitHubTestResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
	}
}

func projectOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/projects", id: "listProjects", tag: "projects",
			summary: "List all registered projects (active + degraded)",
			resps: []respUnit{
				{http.StatusOK, controllers.ListProjectsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects", id: "addProject", tag: "projects",
			summary: "Register a new project from a git repository path",
			reqBody: projectsvc.AddInput{},
			resps: []respUnit{
				{http.StatusCreated, controllers.ProjectResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/initialize", id: "initializeProjectRepository", tag: "projects",
			summary: "Initialize a selected folder as a Git repository with an initial commit",
			reqBody: projectsvc.InitializeRepositoryInput{},
			resps: []respUnit{
				{http.StatusOK, projectsvc.InitializeRepositoryResult{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/projects/browse", id: "browseProjectRoot", tag: "projects",
			summary:    "List the immediate subdirectories of an absolute path under the configured allowed project roots (or the home directory when none are configured), for the web folder-browser UX",
			pathParams: []any{controllers.BrowseProjectRootQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.BrowseProjectRootResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/clone", id: "cloneProjectFromGitHub", tag: "projects",
			summary: "Clone a GitHub repository into an allowed project root and register it",
			reqBody: projectsvc.CloneInput{},
			resps: []respUnit{
				{http.StatusCreated, controllers.ProjectResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		}, {
			method: http.MethodGet, path: "/api/v1/projects/{id}", id: "getProject", tag: "projects",
			summary:    "Fetch one project; discriminates ok vs degraded",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.GetProjectResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/projects/{id}", id: "updateProjectSettings", tag: "projects",
			summary:    "Atomically replace a project's display name and config",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    projectsvc.UpdateSettingsInput{},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/projects/{id}/config", id: "setProjectConfig", tag: "projects",
			summary:    "Replace a project's per-project config",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    projectsvc.SetConfigInput{},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/projects/{id}", id: "removeProject", tag: "projects",
			summary:    "Remove a project; stops sessions, cleans workspaces, unregisters",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, projectsvc.RemoveResult{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/repo-connection-test", id: "testRepoConnection", tag: "projects",
			summary:    "Run a non-destructive git ls-remote probe against a project's root repo or a named workspace child repo",
			pathParams: []any{controllers.ProjectIDParam{}},
			reqBody:    controllers.TestRepoConnectionRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.TestRepoConnectionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/projects/{id}/workspace-repos/refresh", id: "refreshWorkspaceRepos", tag: "projects",
			summary:    "Re-detect a workspace project's child repositories from disk, correcting stale per-repo metadata",
			pathParams: []any{controllers.ProjectIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ProjectResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
	}
}

func sessionOperations() []operation {
	return []operation{
		{
			method: http.MethodGet, path: "/api/v1/sessions", id: "listSessions", tag: "sessions",
			summary:    "List sessions",
			pathParams: []any{controllers.ListSessionsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListSessionsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions", id: "spawnSession", tag: "sessions",
			summary: "Spawn a new agent session",
			reqBody: controllers.SpawnSessionRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.SpawnSessionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}", id: "getSession", tag: "sessions",
			summary:    "Fetch one session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/pin", id: "pinSession", tag: "sessions",
			summary:    "Pin a session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/sessions/{sessionId}/pin", id: "unpinSession", tag: "sessions",
			summary:    "Unpin a session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/preview", id: "getSessionPreview", tag: "sessions",
			summary:    "Discover a browser preview URL for a session workspace",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionPreviewResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/preview", id: "setSessionPreview", tag: "sessions",
			summary:    "Set (or autodetect) the browser preview URL for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionPreviewRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/sessions/{sessionId}/preview", id: "clearSessionPreview", tag: "sessions",
			summary:    "Clear the browser preview URL for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/preview/server", id: "getSessionPreviewServer", tag: "sessions",
			summary:    "Get the managed preview server status for a session",
			pathParams: []any{controllers.SessionIDParam{}, controllers.BrowserCapabilityHeader{}},
			resps: []respUnit{
				{http.StatusOK, controllers.PreviewServerStatusResponse{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/preview/server", id: "startSessionPreviewServer", tag: "sessions",
			summary:    "Start a session-owned server from .ao/launch.json and open its application preview",
			pathParams: []any{controllers.SessionIDParam{}, controllers.BrowserCapabilityHeader{}},
			reqBody:    controllers.StartPreviewServerRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.PreviewServerStatusResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusRequestTimeout, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusGatewayTimeout, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/sessions/{sessionId}/preview/server", id: "stopSessionPreviewServer", tag: "sessions",
			summary:    "Stop the managed preview server for a session",
			pathParams: []any{controllers.SessionIDParam{}, controllers.BrowserCapabilityHeader{}},
			resps: []respUnit{
				{http.StatusOK, controllers.PreviewServerStatusResponse{}},
				{http.StatusForbidden, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/preview/files/*", id: "getSessionPreviewFile", tag: "sessions",
			summary:    "Serve a static browser preview file from a session workspace",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, ""},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
			contentTypes: map[int]string{http.StatusOK: "text/html"},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/attachments", id: "stageSessionAttachments", tag: "sessions",
			summary:    "Write images into a running session's worktree and return their paths",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.StageSessionAttachmentsRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.StageSessionAttachmentsResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/workspace/files", id: "listSessionWorkspaceFiles", tag: "sessions",
			summary:    "List files in a session workspace with git change status",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListWorkspaceFilesResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/workspace/events", id: "streamSessionWorkspaceChanges", tag: "sessions",
			summary:    "Stream session workspace file changes",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, ""},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
			contentTypes: map[int]string{http.StatusOK: "text/event-stream"},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/workspace/file", id: "getSessionWorkspaceFile", tag: "sessions",
			summary:    "Read one session workspace file and its git diff",
			pathParams: []any{controllers.SessionIDParam{}, controllers.WorkspaceFileQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.WorkspaceFileResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/pr", id: "listSessionPRs", tag: "sessions",
			summary:    "List pull requests owned by a session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListSessionPRsResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/pr/claim", id: "claimSessionPR", tag: "sessions",
			summary:    "Claim an existing pull request for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.ClaimPRRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.ClaimPRResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusServiceUnavailable, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/sessions/{sessionId}", id: "renameSession", tag: "sessions",
			summary:    "Rename a session display name",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.RenameSessionRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.RenameSessionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/sessions/{sessionId}/merge-policy", id: "setSessionMergePolicy", tag: "sessions",
			summary:    "Configure whether PR completion terminates the session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionMergePolicyRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SetSessionMergePolicyResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/sessions/{sessionId}/auto-inject-review", id: "setSessionAutoInjectReview", tag: "sessions",
			summary:    "Set the auto-inject review setting for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionAutoInjectReviewRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SetSessionAutoInjectReviewResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPatch, path: "/api/v1/sessions/{sessionId}/auto-inject-ci", id: "setSessionAutoInjectCI", tag: "sessions",
			summary:    "Set the automatic CI-failure injection default for new session PRs",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionAutoInjectCIRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SetSessionAutoInjectCIResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/sessions/{sessionId}/reviewer", id: "setSessionReviewer", tag: "sessions",
			summary:    "Set the reviewer harness for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionReviewerRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPut, path: "/api/v1/sessions/{sessionId}/auto-review", id: "setSessionAutoReview", tag: "sessions",
			summary:    "Enable or disable automatic review for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetSessionAutoReviewRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/cleanup", id: "cleanupSessions", tag: "sessions",
			summary:    "Clean up terminated session workspaces",
			pathParams: []any{controllers.CleanupSessionsQuery{}},
			resps: []respUnit{
				{http.StatusOK, controllers.CleanupSessionsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/restore", id: "restoreSession", tag: "sessions",
			summary:    "Restore a terminated session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.RestoreSessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/resume-agent", id: "resumeAgent", tag: "sessions",
			summary:    "Resume an exited agent in its existing session",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ResumeAgentResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/switch-agent", id: "switchSessionAgent", tag: "sessions",
			summary:    "Switch a logical AO session to another agent harness",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SwitchAgentRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.AgentSwitchResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/agent-switches", id: "listSessionAgentSwitches", tag: "sessions",
			summary:    "List a session's durable agent-switch history",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.ListAgentSwitchesResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/agent-switches/{switchId}/handoff", id: "submitSessionAgentHandoff", tag: "sessions",
			summary:    "Submit a generation-fenced source-agent handoff",
			pathParams: []any{controllers.SessionIDParam{}, controllers.AgentSwitchIDParam{}},
			reqBody:    controllers.SubmitAgentHandoffRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.AgentSwitchResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/sessions/{sessionId}/interface-transition", id: "getSessionInterfaceTransition", tag: "sessions",
			summary:    "Inspect TUI and Chat interface handoff support and progress",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionInterfaceTransitionStatusResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/interface-transition", id: "startSessionInterfaceTransition", tag: "sessions",
			summary:    "Switch a live session between its TUI and Chat controllers",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.StartSessionInterfaceTransitionRequest{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.StartSessionInterfaceTransitionResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodDelete, path: "/api/v1/sessions/{sessionId}/interface-transition", id: "cancelSessionInterfaceTransition", tag: "sessions",
			summary:    "Cancel an interface handoff before its source controller stops",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusAccepted, controllers.CancelSessionInterfaceTransitionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/kill", id: "killSession", tag: "sessions",
			summary:    "Mark a session terminated and tear down runtime/workspace resources",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.KillSessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/rollback", id: "rollbackSession", tag: "sessions",
			summary:    "Undo a partially-completed spawn (delete seed row, or kill if spawn output exists)",
			pathParams: []any{controllers.SessionIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.RollbackSessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/send", id: "sendSessionMessage", tag: "sessions",
			summary:    "Send a message to a running session's agent",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SendSessionMessageRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SendSessionMessageResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				// Conflict: the session is terminated, or paused on a permission
				// decision (SESSION_AWAITING_DECISION) — the guarded send refuses
				// to paste into a pending dialog.
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/sessions/{sessionId}/activity", id: "setSessionActivity", tag: "sessions",
			summary:    "Report an agent activity-state signal for a session",
			pathParams: []any{controllers.SessionIDParam{}},
			reqBody:    controllers.SetActivityRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SetActivityResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/reviews/{reviewSessionID}/activity", id: "setReviewActivity", tag: "reviews",
			summary:    "Report a reviewer-owned hook signal",
			pathParams: []any{controllers.ReviewSessionIDParam{}},
			reqBody:    controllers.SetReviewActivityRequest{},
			resps: []respUnit{
				{http.StatusOK, controllers.SetReviewActivityResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/orchestrators", id: "listOrchestrators", tag: "sessions",
			summary: "List orchestrator sessions across projects",
			resps: []respUnit{
				{http.StatusOK, controllers.ListSessionsResponse{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/orchestrators", id: "spawnOrchestrator", tag: "sessions",
			summary: "Spawn an orchestrator session",
			reqBody: controllers.SpawnOrchestratorRequest{},
			resps: []respUnit{
				{http.StatusCreated, controllers.SpawnOrchestratorResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/orchestrators/delegate", id: "delegateTask", tag: "sessions",
			summary: "Start a worker task and ask the orchestrator to title it",
			reqBody: controllers.DelegateTaskRequest{},
			resps: []respUnit{
				{http.StatusAccepted, controllers.DelegateTaskResponse{}},
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodGet, path: "/api/v1/orchestrators/{id}", id: "getOrchestrator", tag: "sessions",
			summary:    "Fetch one orchestrator session",
			pathParams: []any{controllers.OrchestratorIDParam{}},
			resps: []respUnit{
				{http.StatusOK, controllers.SessionResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusInternalServerError, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}

// prOperations declares the PR action operations. These live in the SCM lane:
// the handler delegates to a PRService backed by the SCM provider. A nil
// PRService (SCM not configured) returns 501 for both routes.
func prOperations() []operation {
	return []operation{
		{
			method: http.MethodPost, path: "/api/v1/prs/{id}/merge", id: "mergePR", tag: "prs",
			summary:    "Squash-merge a pull request",
			pathParams: []any{controllers.PRIDParam{}},
			reqBody:    controllers.MergePRRequest{},
			resps: []respUnit{
				{http.StatusBadRequest, envelope.APIError{}},
				{http.StatusOK, controllers.MergePRResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusConflict, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
		{
			method: http.MethodPost, path: "/api/v1/prs/{id}/resolve-comments", id: "resolveComments", tag: "prs",
			summary:    "Resolve review threads on a pull request",
			pathParams: []any{controllers.PRIDParam{}},
			reqBody:    nil, // body is optional: omitting it resolves all unresolved threads
			resps: []respUnit{
				{http.StatusOK, controllers.ResolveCommentsResponse{}},
				{http.StatusNotFound, envelope.APIError{}},
				{http.StatusUnprocessableEntity, envelope.APIError{}},
				{http.StatusNotImplemented, envelope.APIError{}},
			},
		},
	}
}
