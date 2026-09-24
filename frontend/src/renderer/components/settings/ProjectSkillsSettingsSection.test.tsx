import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { appI18n } from "../../i18n";
import { ProjectSkillsSettingsSection } from "./ProjectSkillsSettingsSection";

const securityAudit = {
	id: "security-audit",
	version: "0.1.0",
	name: "Security Audit",
	description: "On-demand security review of one project.",
	riskLevel: "critical",
	originType: "builtin",
	publisher: "agent-orchestrator",
	digest: "abc123",
	capabilities: ["repo.read", "report.write", "net.egress"],
	requiresIsolatedRunner: true,
	approval: "per_activation",
	installedAt: new Date().toISOString(),
	modes: [
		{ id: "static-code", name: "Static code review", description: "", riskLevel: "low", capabilities: ["repo.read"], approval: "per_activation" },
		{ id: "dependencies", name: "Dependency review", description: "", riskLevel: "medium", capabilities: ["net.egress"], approval: "per_run" },
	],
};

const capabilityPolicy = [
	{
		name: "repo.read", description: "Read the project's source in a checkout.",
		risk: "low", minApproval: "none", requiresControls: [],
		requiredPermission: "project.read",
	},
	{
		name: "report.write", description: "Write a structured report.",
		risk: "low", minApproval: "none", requiresControls: [],
		requiredPermission: "project.read",
	},
	{
		name: "net.egress", description: "Open outbound network connections.",
		risk: "high", minApproval: "per_run",
		requiresControls: ["filesystem_isolation", "egress_allowlist"],
		requiredPermission: "project.manage",
	},
];

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<ProjectSkillsSettingsSection projectId="medusa" />
		</QueryClientProvider>,
	);
}

function mockSkills({
	permissions,
	activations,
	runDetail,
	runs = [],
}: {
	permissions: string[];
	activations: unknown[];
	runDetail?: unknown;
	runs?: unknown[];
}) {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/projects/{id}/skills") {
			return {
				data: { projectId: "medusa", installed: [securityAudit], activations, permissions },
			} as never;
		}
		if (path === "/api/v1/projects/{id}/skills/runs/{runId}") {
			return { data: runDetail } as never;
		}
		if (path === "/api/v1/projects/{id}/skills/runs") {
			return { data: { runs } } as never;
		}
		return { data: { skills: [securityAudit], capabilities: capabilityPolicy } } as never;
	}) as never);
}

const runSummary = (state: string, extra: Record<string, unknown> = {}) => ({
	id: "skr-1", projectId: "medusa", skillId: "security-audit", version: "0.1.0",
	modeId: "static-code", tool: "ao.static-scan/v1", state, requestedBy: "ada",
	inputs: {}, capabilities: ["repo.read", "report.write"], runnerId: "container/docker",
	runnerControls: [], packageDigest: "abc123", summary: "", findingCount: 0,
	truncated: false, cancelRequested: false, createdAt: new Date().toISOString(),
	...extra,
});

const startedRun = { data: { run: runSummary("queued"), created: true } };

const succeededDetail = (report: unknown) => ({
	run: runSummary("succeeded", { reportSha256: "f00d", durationMs: 1200 }),
	findings: [],
	report,
	integrity: "verified",
});

const enabledActivation = {
	skillId: "security-audit",
	skillName: "Security Audit",
	version: "0.1.0",
	enabled: true,
	grantedCapabilities: ["repo.read", "report.write"],
	requestedCapabilities: securityAudit.capabilities,
	approvedBy: "ada",
	updatedAt: new Date().toISOString(),
	available: true,
};

describe("ProjectSkillsSettingsSection", () => {
	afterEach(async () => {
		vi.restoreAllMocks();
		await appI18n.changeLanguage("en");
	});

	it("shows what this project enabled, with the grant and who approved it", async () => {
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		renderSection();

		expect(await screen.findByText("Security Audit")).toBeInTheDocument();
		expect(screen.getByText("Enabled")).toBeInTheDocument();
		expect(screen.getByText("Granted: repo.read, report.write")).toBeInTheDocument();
		expect(screen.getByText("Approved by ada")).toBeInTheDocument();
	});

	// A viewer sees the state and no controls. The screen renders from the
	// caller's own permissions, so this is the same decision the daemon makes.
	it("renders read-only without project.manage", async () => {
		mockSkills({ permissions: ["project.read"], activations: [enabledActivation] });
		renderSection();

		expect(await screen.findByText("Security Audit")).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Disable" })).not.toBeInTheDocument();
		expect(screen.queryByRole("combobox", { name: "Choose a skill to enable" })).not.toBeInTheDocument();
	});

	// The risk copy and the gating permission come from the daemon's policy,
	// never from a table in React.
	it("renders the capability policy from the server and refuses a grant the caller cannot make", async () => {
		mockSkills({ permissions: ["project.read"], activations: [] });
		// A caller who may activate but holds only project.read.
		vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
			if (path === "/api/v1/projects/{id}/skills") {
				return {
					data: {
						projectId: "medusa",
						installed: [securityAudit],
						activations: [],
						permissions: ["project.read", "project.manage"],
					},
				} as never;
			}
			return { data: { skills: [securityAudit], capabilities: capabilityPolicy } } as never;
		}) as never);

		renderSection();
		const user = userEvent.setup();
		await user.click(await screen.findByRole("combobox", { name: "Choose a skill to enable" }));
		await user.click(await screen.findByRole("option", { name: /Security Audit/ }));

		expect(await screen.findByText("Read the project's source in a checkout.")).toBeInTheDocument();
		expect(screen.getByText("Open outbound network connections.")).toBeInTheDocument();
		// net.egress needs an isolated runner AO does not have; the screen says
		// so rather than offering it as if it would work.
		// The capability names the controls it needs, so the reason reads as
		// "this has to be built" rather than a vague "not isolated".
		expect(
			screen.getByText(
				"Needs from the runner: filesystem_isolation, egress_allowlist. Blocked until those exist.",
			),
		).toBeInTheDocument();
		expect(screen.getByText("Grant only the capabilities this project actually needs. Anything you leave off is refused at run time.")).toBeInTheDocument();
	});

	// The one "run"-shaped control, and it runs nothing.
	it("reports a blocked dry run with the missing runner named", async () => {
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		const post = vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: {
				skillId: "security-audit",
				version: "0.1.0",
				skillName: "Security Audit",
				modeId: "static-code",
				modeName: "Static code review",
				modeRisk: "low",
				verdict: "blocked",
				requiredApproval: "per_run",
				decisions: [
					{
						capability: "repo.read", satisfied: true, risk: "low",
						description: "Read the project's source in a checkout.",
						requiredPermission: "project.read", requiresControls: [],
					},
					{
						capability: "net.egress", satisfied: false, risk: "high",
						description: "Open outbound network connections.",
						requiredPermission: "project.manage",
						denialReason: "missing_control",
						missingControl: "egress_allowlist",
						requiresControls: ["filesystem_isolation", "egress_allowlist"],
						detail: "the execution environment (container/docker) does not provide egress_allowlist",
					},
				],
				missingPermissions: ["project.manage"],
				effectiveRisk: "high",
				runner: {
					runnerId: "container/docker", available: true, isolated: true,
					egressControlled: false,
					controls: ["filesystem_isolation", "process_isolation", "egress_deny_all"],
					missingControls: ["egress_allowlist"],
					needsIsolation: true, needsEgressControl: true,
				},
				reasons: [
					"net.egress: the execution environment (container/docker) does not provide egress_allowlist",
				],
			},
		} as never);

		renderSection();
		const user = userEvent.setup();
		await user.click(await screen.findByRole("button", { name: "Check what a run needs" }));

		expect(await screen.findByText("Blocked")).toBeInTheDocument();
		expect(screen.getByText("This check starts no process and changes nothing.")).toBeInTheDocument();
		expect(
			screen.getByText(
				"the execution environment (container/docker) does not provide egress_allowlist",
			),
		).toBeInTheDocument();
		expect(screen.getByText("You are missing: project.manage")).toBeInTheDocument();
		// The panel says what the environment IS and what is still missing.
		// "Blocked" without the gap is not actionable.
		expect(
			screen.getByText(
				"Runner container/docker provides: filesystem_isolation, process_isolation, egress_deny_all.",
			),
		).toBeInTheDocument();
		expect(screen.getByText("Missing for this run: egress_allowlist.")).toBeInTheDocument();
		expect(post).toHaveBeenCalledOnce();
	});

	// An activation whose package went missing is enabled and unusable at the
	// same time; showing only the flag would report the more flattering fact.
	it("surfaces an enabled-but-unusable activation", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"],
			activations: [
				{
					...enabledActivation,
					available: false,
					unavailable: "integrity.digest does not match package contents",
				},
			],
		});
		renderSection();

		expect(
			await screen.findByText(
				"Enabled but unusable: integrity.digest does not match package contents",
			),
		).toBeInTheDocument();
	});

	it("localizes into Spanish", async () => {
		mockSkills({ permissions: ["project.read"], activations: [enabledActivation] });
		await appI18n.changeLanguage("es");
		renderSection();

		expect(await screen.findByText("Habilitada")).toBeInTheDocument();
		expect(screen.getByText("Concedido: repo.read, report.write")).toBeInTheDocument();
		await waitFor(() =>
			expect(
				screen.getByText(
					"Skills instaladas en esta instalación y cuáles ha habilitado este proyecto. Habilitar una Skill nunca la ejecuta.",
				),
			).toBeInTheDocument(),
		);
	});
});

// The execution surface, and the rule the report is built around.
describe("ProjectSkillsSettingsSection — running", () => {
	afterEach(() => vi.restoreAllMocks());

	const executableDryRun = {
		skillId: "security-audit", version: "0.1.0", skillName: "Security Audit",
		modeId: "static-code", modeName: "Static code review", modeRisk: "low",
		verdict: "executable", requiredApproval: "per_activation",
		decisions: [{
			capability: "repo.read", satisfied: true, risk: "low",
			description: "Read the project's source in a checkout.",
			requiredPermission: "project.read", requiresControls: [],
		}],
		missingPermissions: [], effectiveRisk: "low",
		runner: {
			runnerId: "container/docker", available: true, isolated: true,
			egressControlled: false, controls: ["filesystem_isolation"], missingControls: [],
			needsIsolation: true, needsEgressControl: false,
		},
		reasons: [],
	};

	// Run is offered only after a dry run says the mode is executable. A Run
	// button beside a blocked mode invites a click whose only outcome is an
	// error, and the dry run is the surface that explains why.
	it("offers Run only once a dry run says the mode is executable", async () => {
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: { ...executableDryRun, verdict: "blocked" },
		} as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await screen.findByTestId("project-skill-dry-run");
		expect(screen.queryByTestId("project-skill-run")).not.toBeInTheDocument();
	});

	it("renders coverage before findings, and names the bytes that ran", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: succeededDetail({
			imageDigest: "sha256:dddd", approvalId: "skimg-9", approvedBy: "ada",
			coverage: {
				filesStaged: 12, filesVisible: 12, filesScanned: 10,
				skipped: [{ path: "vendor/big.bin", reason: "binary" }],
				limitations: ["This is a pattern scanner, not a static analyzer."],
			},
			findings: [{
				ruleId: "hardcoded-secret", severity: "high", category: "secrets",
				title: "Possible hardcoded credential", path: "src/db.go", line: 42,
				recommendation: "Move it to a secret store.", confidence: "possible",
			}],
		}),
		});
		const post = vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		const report = await screen.findByTestId("project-skill-run-report");
		expect(post).toHaveBeenCalledWith(
			"/api/v1/projects/{id}/skills/{skillId}/run",
			expect.objectContaining({
				params: { path: { id: "medusa", skillId: "security-audit" } },
			}),
		);
		// Which bytes ran and who allowed them.
		expect(report).toHaveTextContent("sha256:dddd");
		expect(report).toHaveTextContent("ada");
		// Coverage strictly before findings.
		const text = report.textContent ?? "";
		expect(text.indexOf("Coverage:")).toBeGreaterThanOrEqual(0);
		expect(text.indexOf("Coverage:")).toBeLessThan(text.indexOf("finding"));
		expect(report).toHaveTextContent("12 staged, 12 visible, 10 scanned");
		expect(report).toHaveTextContent("Skipped vendor/big.bin (binary)");
		expect(report).toHaveTextContent("[HIGH] Possible hardcoded credential");
		// The tool saying what it cannot know, next to the findings.
		expect(report).toHaveTextContent("pattern scanner");
	});

	// An agent run's report is findings.v1, not a static scan: it must render as
	// a host agent's reading, never with container or image evidence it never had.
	it("renders an agent run's report as the agent's reading, coverage first", async () => {
		const detail = succeededDetail({
			schemaVersion: "security-audit/findings/v1",
			coverage: {
				examined: ["api/orders.go", "api/router.go"],
				skipped: [{ path: ".env", reason: "excluded by AO before staging: denied-by-manifest" }],
			},
			findings: [{
				id: "AUTHZ-2", title: "GetOrder skips the tenant predicate", severity: "critical",
				confidence: "confirmed", category: "idor",
				evidence: { locations: [{ path: "api/orders.go", line: 12 }] },
				recommendation: "Add the tenant predicate.",
			}],
		});
		detail.run = { ...detail.run, tool: "ao.skill-agent/v1", runnerId: "host-agent/claude-code" };
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: detail,
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		const report = await screen.findByTestId("project-skill-run-report");
		expect(report).toHaveTextContent("Read by host-agent/claude-code over a read-only staged copy");
		expect(report).toHaveTextContent("not a container");
		const text = report.textContent ?? "";
		expect(text.indexOf("Coverage: 2 examined, 1 excluded")).toBeGreaterThanOrEqual(0);
		expect(text.indexOf("Coverage:")).toBeLessThan(text.indexOf("finding"));
		expect(report).toHaveTextContent("Skipped .env");
		expect(report).toHaveTextContent("[CRITICAL] GetOrder skips the tenant predicate");
		expect(report).toHaveTextContent("api/orders.go:12 · AUTHZ-2 · confirmed");
		expect(report).not.toHaveTextContent("Ran ");
		expect(report).not.toHaveTextContent("staged, ");
	});

	// A finding about a whole FILE (a denied credential file seen, never read)
	// has no line; it must not render as ":0".
	it("renders a file-level finding without a line number", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: succeededDetail({
				imageDigest: "sha256:dddd", approvalId: "skimg-9", approvedBy: "ada",
				coverage: { filesStaged: 3, filesVisible: 3, filesScanned: 3 },
				findings: [{
					ruleId: "SEC-100", severity: "medium", category: "secret",
					title: "Credential-bearing file present in the checkout (not read)", path: ".env", line: 0,
					recommendation: "Confirm it is not committed.", confidence: "possible",
				}],
			}),
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		const report = await screen.findByTestId("project-skill-run-report");
		expect(report).toHaveTextContent(".env · SEC-100 · possible");
		expect(report).not.toHaveTextContent(".env:0");
	});

	// The failure this panel exists to prevent.
	it("says an empty scan is not a clean result", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: succeededDetail({
			imageDigest: "sha256:dddd", approvalId: "skimg-9", approvedBy: "ada",
			coverage: { filesStaged: 0, filesVisible: 0, filesScanned: 0 },
			findings: [],
		}),
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		const report = await screen.findByTestId("project-skill-run-report");
		expect(report).toHaveTextContent("Nothing was scanned, so nothing was found");
		expect(report).toHaveTextContent("This is not a clean result");
	});

	// AO does not stop a running container, so a revocation mid-run is a fact
	// about the results and has to reach the reader.
	it("reports an approval revoked while the run was in flight", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: succeededDetail({
			imageDigest: "sha256:dddd", approvalId: "skimg-9", approvedBy: "ada",
			approvalRevokedDuringRun: true,
			coverage: { filesStaged: 3, filesVisible: 3, filesScanned: 3 },
			findings: [],
		}),
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		expect(await screen.findByTestId("project-skill-run-report"))
			.toHaveTextContent("revoked while this was running");
	});

	// A refused run must not render an empty report that reads as a completed
	// scan with nothing found.
	it("surfaces a refusal instead of an empty report", async () => {
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return { error: { message: "no image is approved for this scope" } } as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		await waitFor(() =>
			expect(screen.getByText(/no image is approved for this scope/)).toBeInTheDocument(),
		);
		expect(screen.queryByTestId("project-skill-run-report")).not.toBeInTheDocument();
	});

	it("follows a run in progress and offers to cancel it", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: { run: runSummary("running"), findings: [], integrity: "none" },
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			if (path.endsWith("/cancel")) return { data: runSummary("running", { cancelRequested: true }) } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		expect(await screen.findByTestId("project-skill-run-progress")).toHaveTextContent("skr-1");
		expect(screen.queryByTestId("project-skill-run-report")).not.toBeInTheDocument();
		await userEvent.click(screen.getByTestId("project-skill-run-cancel"));
		await waitFor(() =>
			expect(apiClient.POST).toHaveBeenCalledWith(
				"/api/v1/projects/{id}/skills/runs/{runId}/cancel",
				expect.objectContaining({ params: { path: { id: "medusa", runId: "skr-1" } } }),
			),
		);
	});

	// A run that ended without a report says how it ended, with the code.
	it("says how an unsuccessful run ended", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: {
				run: runSummary("refused", { errorCode: "SKILL_IMAGE_NOT_APPROVED", errorMessage: "no image is approved" }),
				findings: [], integrity: "none",
			},
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		expect(await screen.findByTestId("project-skill-run-ended")).toHaveTextContent("SKILL_IMAGE_NOT_APPROVED");
		expect(screen.queryByTestId("project-skill-run-report")).not.toBeInTheDocument();
	});

	// A report whose stored bytes no longer match their digest is never shown.
	it("does not show a report that failed verification", async () => {
		mockSkills({
			permissions: ["project.read", "project.manage"], activations: [enabledActivation],
			runDetail: { run: runSummary("succeeded", { reportSha256: "f00d" }), findings: [], integrity: "mismatch" },
		});
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return startedRun as never;
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Check what a run needs" }));
		await userEvent.click(await screen.findByTestId("project-skill-run"));

		expect(await screen.findByTestId("project-skill-run-unverified")).toBeInTheDocument();
		expect(screen.queryByTestId("project-skill-run-report")).not.toBeInTheDocument();
	});

	it("lists the project's run history, including runs that did not succeed", async () => {
		mockSkills({
			permissions: ["project.read"], activations: [enabledActivation],
			runs: [
				runSummary("succeeded", { id: "skr-2", summary: "scanned 2 of 3 staged files, 1 findings", durationMs: 1500 }),
				runSummary("failed", { id: "skr-1", errorCode: "SKILL_RUN_INTERRUPTED", errorMessage: "the daemon stopped" }),
			],
		});
		renderSection();

		const history = await screen.findByTestId("project-skill-runs");
		await waitFor(() => expect(screen.getAllByTestId("project-skill-run-row")).toHaveLength(2));
		expect(history).toHaveTextContent("scanned 2 of 3 staged files");
		expect(history).toHaveTextContent("SKILL_RUN_INTERRUPTED");
		expect(history).toHaveTextContent("1.5s");
	});
});
