import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import type { VerificationPlan } from "../lib/task-verification";

export type WorkflowRunView = components["schemas"]["WorkflowRunView"];

/**
 * The canonical execution strategies a run may be created under. "auto" is a
 * valid API request value too, but the create form always states a choice, so
 * it is deliberately not offered here.
 */
export const EXECUTION_STRATEGIES = ["task", "autonomous", "master"] as const;
export type ExecutionStrategy = (typeof EXECUTION_STRATEGIES)[number];

/** Approval is a separate axis from strategy: who approves and drives the run. */
export const APPROVAL_POLICIES = ["automatic", "manual"] as const;
export type ApprovalPolicy = (typeof APPROVAL_POLICIES)[number];

/**
 * P1-B: the run's frozen auto-repair policy. A third independent axis — it
 * decides what AO may do to a run unattended when a repairable technical stop
 * happens, and it is frozen at creation like the other two.
 */
export const REPAIR_POLICIES = ["disabled", "suggest", "automatic"] as const;
export type RepairPolicy = (typeof REPAIR_POLICIES)[number];

/**
 * P3-A §7: where the work happens, chosen at creation.
 *
 * The order is the order the options are offered in, and it puts the two
 * explicit answers before the deferral on purpose: `auto` is what AO does when
 * nobody has an opinion, not the recommended answer.
 *
 * `direct_branch` is BINDING. A run created with it never silently becomes an
 * isolated worktree — if the branch cannot be used, AO waits or stops with the
 * cause named. That guarantee lives in the daemon; the renderer only has to
 * stop describing the choice as a preference.
 */
export const PLACEMENTS = ["direct_branch", "isolated_worktree", "auto"] as const;
export type Placement = (typeof PLACEMENTS)[number];

/**
 * P5-A: how much scrutiny this run asks its delivered changes to receive.
 *
 * The order is cheapest first, which is also the order of increasing cost, so
 * the list reads as a dial rather than as three unrelated options.
 *
 * It is a REQUEST, not a guarantee. The daemon reviews every change at the
 * deeper of this request and that change's own deterministic risk tier, so a
 * change touching security, authentication, payments, migrations, concurrency,
 * infrastructure, a public contract or dependency configuration gets a full
 * independent review whatever is selected here. The renderer only has to stop
 * describing the choice as a ceiling.
 *
 * `auto` is not offered: the daemon's default is per-strategy, and the form
 * already shows which strategy is selected, so an explicit dial is clearer than
 * a fourth option meaning "whatever the other radio group implies".
 */
export const REVIEW_DEPTHS = ["none", "light", "deep"] as const;
export type ReviewDepth = (typeof REVIEW_DEPTHS)[number];

/**
 * The depth each execution strategy asks for when nobody chose one. It mirrors
 * domain.DefaultRequestedReviewDepth, and it is duplicated here only so the
 * form can pre-select the honest default as the strategy radio changes — the
 * daemon still freezes its own default and would ignore a disagreeing renderer.
 */
export const DEFAULT_REVIEW_DEPTH: Record<ExecutionStrategy, ReviewDepth> = {
	task: "none",
	autonomous: "light",
	master: "deep",
};

export function workflowRunsQueryKey(projectId?: string) {
	return ["workflow-runs", projectId ?? ""] as const;
}

/**
 * Lists workflow run summaries, optionally filtered by project, and exposes a
 * create mutation. Checkpoint 8A: this is structure only — creating a run
 * seeds its initial steps but nothing executes them yet.
 */
export function useWorkflowRuns(projectId?: string) {
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: workflowRunsQueryKey(projectId),
		enabled: hasTrustedApiBaseUrl(),
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/workflows", {
				params: { query: projectId ? { projectId } : {} },
			});
			if (error) throw error;
			return data.workflows;
		},
	});

	const create = useMutation({
		mutationFn: async (input: {
			projectId: string;
			objective: string;
			strategy: ExecutionStrategy;
			approvalPolicy: ApprovalPolicy;
			repairPolicy: RepairPolicy;
			placement: Placement;
			reviewDepth: ReviewDepth;
			// A TASK run has no planner, so the two things a planner would
			// otherwise have produced come from the form. Both are optional in
			// the request; the daemon refuses a task whose verification cannot
			// be executed, which is why the form requires one before it gets
			// this far.
			acceptanceCriteria?: string[];
			verification?: VerificationPlan;
		}) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{projectId}/workflows", {
				params: { path: { projectId: input.projectId } },
				body: {
					objective: input.objective,
					// P1-A: strategy and approval are two independent axes, and the
					// daemon owns both decisions. The renderer no longer derives
					// masterPlan/planApprovalMode/autonomous itself — those were the
					// implicit flags the execution-strategy model replaces.
					strategy: input.strategy,
					approvalPolicy: input.approvalPolicy,
					repairPolicy: input.repairPolicy,
					// P3-A §7: the placement travels with the create request, so it
					// is recorded durably BEFORE anything this run can execute and
					// the freeze consumes it. Sending it afterwards would race the
					// autonomous kickoff, and losing that race is exactly how an
					// explicit "current branch" became a worktree nobody asked for.
					placement: input.placement,
					// P5-A: the review-depth request travels with the create
					// request for the same reason placement does — it is frozen
					// before anything can execute. It cannot make a review
					// shallower than the change's risk tier allows; that clamp is
					// the daemon's and is not expressible here.
					reviewDepth: input.reviewDepth,
					// The task's acceptance criteria and its verification plan,
					// sent verbatim. They are bound into the run's plan artifact
					// by the SAME transaction that creates it, so a task can
					// never durably exist without the checks it was asked for.
					// Omitted entirely when empty: an absent field leaves the
					// daemon's own default criteria in place, where an empty
					// array would replace them with nothing.
					...(input.acceptanceCriteria && input.acceptanceCriteria.length > 0
						? { acceptanceCriteria: input.acceptanceCriteria }
						: {}),
					...(input.verification ? { verification: input.verification } : {}),
				},
			});
			if (error) throw error;
			return data.workflow;
		},
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: workflowRunsQueryKey(projectId) });
		},
	});

	return {
		runs: query.data ?? [],
		isLoading: query.isLoading,
		error: query.error ? apiErrorMessage(query.error) : undefined,
		createRun: create.mutateAsync,
		creating: create.isPending,
		createError: create.error ? apiErrorMessage(create.error) : undefined,
		resetCreateError: create.reset,
	};
}
