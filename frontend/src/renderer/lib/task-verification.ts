import type { components } from "../../api/schema";

export type VerificationPlan = components["schemas"]["WorkflowVerificationPlan"];
export type VerificationCommand = components["schemas"]["WorkflowVerificationCommand"];

/**
 * The form's model of one verification command, held as the strings the user
 * typed rather than as the parsed check.
 *
 * A Task run has no planner, so the checks that decide whether it succeeded can
 * only come from the person creating it. AO does not read them out of the
 * objective prose — a command inferred from a sentence is a command nobody
 * chose — so this is a structured editor and every field below is something the
 * user stated.
 */
export type VerificationCommandDraft = {
	id: string;
	command: string;
	/** Whitespace-separated. See parseCommandArgs for why that is the whole rule. */
	args: string;
	workingDirectory: string;
	timeoutSeconds: string;
	requiredExitCode: string;
	retrySafe: boolean;
};

/** Mirrors the daemon's own ceiling in workflow.VerificationPlan.validate. */
export const MAX_VERIFY_TIMEOUT_SECONDS = 3600;

let nextDraftId = 0;

export function newCommandDraft(): VerificationCommandDraft {
	nextDraftId += 1;
	return {
		id: `verify-command-${nextDraftId}`,
		command: "",
		args: "",
		workingDirectory: "",
		timeoutSeconds: "",
		requiredExitCode: "",
		// Retry-safe by default: the overwhelmingly common check (a test or a
		// build) is one AO may run twice, and a plan whose commands are all
		// retry-safe is the one AO can resolve an ambiguous result for.
		retrySafe: true,
	};
}

/**
 * Arguments are split on whitespace and passed through unchanged. No shell
 * interprets them, so there is deliberately no quote, glob or variable
 * handling to get subtly wrong: an argument containing a space cannot be
 * expressed here, and inventing a quoting dialect to allow it would be a
 * guess about what the user meant.
 */
export function parseCommandArgs(raw: string): string[] {
	return raw.split(/\s+/).filter((part) => part.length > 0);
}

/**
 * A row nobody has typed into. It is not an error — the editor always offers an
 * empty row to type the next command into — it simply contributes no check.
 */
export function isBlankCommandDraft(draft: VerificationCommandDraft): boolean {
	return (
		draft.command.trim() === "" &&
		draft.args.trim() === "" &&
		draft.workingDirectory.trim() === "" &&
		draft.timeoutSeconds.trim() === "" &&
		draft.requiredExitCode.trim() === ""
	);
}

/**
 * The problems the renderer can honestly detect on its own: a command with no
 * name, a working directory that leaves the workspace, a timeout outside the
 * daemon's range, an exit code that is not a number.
 *
 * It deliberately stops there. Which executables a verification may run is the
 * daemon's safety policy (workflow.ValidateVerifyCommand), and a second copy of
 * that list here would drift from the one that is actually enforced. A command
 * this accepts and the daemon refuses comes back as a VERIFICATION_REQUIRED
 * error the form shows, which is the honest answer.
 */
export type CommandDraftProblem =
	| "commandRequired"
	| "workingDirectoryOutside"
	| "timeoutRange"
	| "exitCodeInvalid";

export function commandDraftProblem(draft: VerificationCommandDraft): CommandDraftProblem | undefined {
	if (isBlankCommandDraft(draft)) return undefined;
	if (draft.command.trim() === "") return "commandRequired";
	if (leavesWorkspace(draft.workingDirectory)) return "workingDirectoryOutside";
	const timeout = draft.timeoutSeconds.trim();
	if (timeout !== "") {
		const seconds = Number(timeout);
		if (!Number.isInteger(seconds) || seconds < 0 || seconds > MAX_VERIFY_TIMEOUT_SECONDS) return "timeoutRange";
	}
	const exitCode = draft.requiredExitCode.trim();
	if (exitCode !== "" && !Number.isInteger(Number(exitCode))) return "exitCodeInvalid";
	return undefined;
}

/** Mirrors filepath.IsAbs/Clean's verdict for the daemon's workspace rule. */
function leavesWorkspace(raw: string): boolean {
	const trimmed = raw.trim();
	if (trimmed === "") return false;
	if (trimmed.startsWith("/") || trimmed.startsWith("\\") || /^[A-Za-z]:[\\/]/.test(trimmed)) return true;
	const segments: string[] = [];
	for (const segment of trimmed.split(/[\\/]/)) {
		if (segment === "" || segment === ".") continue;
		if (segment === "..") {
			if (segments.length > 0 && segments[segments.length - 1] !== "..") segments.pop();
			else segments.push("..");
			continue;
		}
		segments.push(segment);
	}
	return segments[0] === "..";
}

/**
 * The drafts that will become checks: every row somebody typed into. An invalid
 * row is still counted here, because the caller must be able to tell "no checks
 * yet" from "a check that is not finished".
 */
export function statedCommandDrafts(drafts: VerificationCommandDraft[]): VerificationCommandDraft[] {
	return drafts.filter((draft) => !isBlankCommandDraft(draft));
}

/**
 * Whether this is a plan the daemon will accept: at least one stated command,
 * and nothing stated that is malformed. It is the renderer's half of the same
 * rule the create route enforces — the form refuses to send a Task that cannot
 * be verified rather than letting the run be created and die at Verify.
 */
export function verificationIsExecutable(drafts: VerificationCommandDraft[]): boolean {
	const stated = statedCommandDrafts(drafts);
	return stated.length > 0 && stated.every((draft) => commandDraftProblem(draft) === undefined);
}

/**
 * Builds the wire plan from the drafts. Blank rows drop out; everything else is
 * sent exactly as typed — nothing is normalised, reordered or added.
 */
export function buildVerificationPlan(drafts: VerificationCommandDraft[]): VerificationPlan {
	const commands: VerificationCommand[] = statedCommandDrafts(drafts).map((draft) => {
		const command: VerificationCommand = {
			command: draft.command.trim(),
			requiredExitCode: draft.requiredExitCode.trim() === "" ? 0 : Number(draft.requiredExitCode.trim()),
			retrySafe: draft.retrySafe,
		};
		const args = parseCommandArgs(draft.args);
		if (args.length > 0) command.args = args;
		const workingDirectory = draft.workingDirectory.trim();
		if (workingDirectory !== "") command.workingDirectory = workingDirectory;
		const timeout = draft.timeoutSeconds.trim();
		if (timeout !== "") command.timeoutSeconds = Number(timeout);
		return command;
	});
	return { commands };
}

/**
 * Acceptance criteria, one per line. Blank lines are dropped; nothing else is
 * touched, so a criterion reaches the daemon as the sentence the user wrote.
 * An empty list is sent as nothing at all, which leaves the daemon's own
 * default criteria in place rather than overwriting them with silence.
 */
export function parseAcceptanceCriteria(raw: string): string[] {
	return raw
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line.length > 0);
}
