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
		risk: "low", minApproval: "none", requiresIsolation: false,
		requiresEgressControl: false, requiredPermission: "project.read",
	},
	{
		name: "report.write", description: "Write a structured report.",
		risk: "low", minApproval: "none", requiresIsolation: false,
		requiresEgressControl: false, requiredPermission: "project.read",
	},
	{
		name: "net.egress", description: "Open outbound network connections.",
		risk: "high", minApproval: "per_run", requiresIsolation: true,
		requiresEgressControl: true, requiredPermission: "project.manage",
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
		expect(
			screen.getByText(
				"Needs an isolated runner, which AO does not have yet, so it stays blocked at run time.",
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
						requiredPermission: "project.read",
					},
					{
						capability: "net.egress", satisfied: false, risk: "high",
						description: "Open outbound network connections.",
						requiredPermission: "project.manage",
						denialReason: "needs_isolated_runner",
						detail: "no runner attests an isolated execution environment",
					},
				],
				missingPermissions: ["project.manage"],
				effectiveRisk: "high",
				runner: {
					runnerId: "none", available: false, isolated: false,
					egressControlled: false, needsIsolation: true, needsEgressControl: true,
				},
				reasons: ["net.egress: no runner attests an isolated execution environment"],
			},
		} as never);

		renderSection();
		const user = userEvent.setup();
		await user.click(await screen.findByRole("button", { name: "Check what a run needs" }));

		expect(await screen.findByText("Blocked")).toBeInTheDocument();
		expect(screen.getByText("This check starts no process and changes nothing.")).toBeInTheDocument();
		expect(screen.getByText("no runner attests an isolated execution environment")).toBeInTheDocument();
		expect(screen.getByText("You are missing: project.manage")).toBeInTheDocument();
		expect(
			screen.getByText(
				"This mode needs an isolated runner with controlled network egress. AO has none, so it stays blocked.",
			),
		).toBeInTheDocument();
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
