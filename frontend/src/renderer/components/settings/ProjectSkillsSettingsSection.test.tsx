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
}: {
	permissions: string[];
	activations: unknown[];
}) {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/projects/{id}/skills") {
			return {
				data: { projectId: "medusa", installed: [securityAudit], activations, permissions },
			} as never;
		}
		return { data: { skills: [securityAudit], capabilities: capabilityPolicy } } as never;
	}) as never);
}

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
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		const post = vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return {
				data: {
					skillId: "security-audit", version: "0.1.0", modeId: "static-code",
					tool: "ao.static-scan/v1",
					report: {
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
					},
				},
			} as never;
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

	// The failure this panel exists to prevent.
	it("says an empty scan is not a clean result", async () => {
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return {
				data: {
					skillId: "security-audit", version: "0.1.0", modeId: "static-code",
					tool: "ao.static-scan/v1",
					report: {
						imageDigest: "sha256:dddd", approvalId: "skimg-9", approvedBy: "ada",
						coverage: { filesStaged: 0, filesVisible: 0, filesScanned: 0 },
						findings: [],
					},
				},
			} as never;
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
		mockSkills({ permissions: ["project.read", "project.manage"], activations: [enabledActivation] });
		vi.spyOn(apiClient, "POST").mockImplementation((async (path: string) => {
			if (path.endsWith("/dry-run")) return { data: executableDryRun } as never;
			return {
				data: {
					skillId: "security-audit", version: "0.1.0", modeId: "static-code",
					tool: "ao.static-scan/v1",
					report: {
						imageDigest: "sha256:dddd", approvalId: "skimg-9", approvedBy: "ada",
						approvalRevokedDuringRun: true,
						coverage: { filesStaged: 3, filesVisible: 3, filesScanned: 3 },
						findings: [],
					},
				},
			} as never;
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
});
