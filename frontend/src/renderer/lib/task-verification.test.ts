import { describe, expect, it } from "vitest";
import {
	buildVerificationPlan,
	commandDraftProblem,
	isBlankCommandDraft,
	newCommandDraft,
	parseAcceptanceCriteria,
	parseCommandArgs,
	statedCommandDrafts,
	verificationIsExecutable,
	type VerificationCommandDraft,
} from "./task-verification";

function draft(fields: Partial<VerificationCommandDraft>): VerificationCommandDraft {
	return { ...newCommandDraft(), ...fields };
}

describe("parseCommandArgs", () => {
	it("splits on whitespace and drops the gaps", () => {
		expect(parseCommandArgs("  test   ./...  -count=1 ")).toEqual(["test", "./...", "-count=1"]);
		expect(parseCommandArgs("test\t./...\nrun")).toEqual(["test", "./...", "run"]);
	});

	it("has no quoting dialect, so nothing is silently reinterpreted", () => {
		// A quoted argument is not a special case here: it arrives as the two
		// tokens it looks like. Inventing quote handling would be a guess about
		// what the user meant, and the guess would run against their repository.
		expect(parseCommandArgs('-run "Test A"')).toEqual(["-run", '"Test', 'A"']);
	});

	it("is empty for empty input", () => {
		expect(parseCommandArgs("")).toEqual([]);
		expect(parseCommandArgs("   ")).toEqual([]);
	});
});

describe("blank rows", () => {
	it("treats an untouched row as contributing nothing rather than as an error", () => {
		const blank = newCommandDraft();
		expect(isBlankCommandDraft(blank)).toBe(true);
		expect(commandDraftProblem(blank)).toBeUndefined();
		expect(statedCommandDrafts([blank])).toEqual([]);
		expect(verificationIsExecutable([blank])).toBe(false);
	});

	it("counts a row with anything in it, even if that is not the command", () => {
		const partial = draft({ args: "test ./..." });
		expect(isBlankCommandDraft(partial)).toBe(false);
		expect(commandDraftProblem(partial)).toBe("commandRequired");
		expect(verificationIsExecutable([partial])).toBe(false);
	});
});

describe("commandDraftProblem", () => {
	it("accepts a plain command", () => {
		expect(commandDraftProblem(draft({ command: "go", args: "test ./..." }))).toBeUndefined();
	});

	it("refuses a working directory that leaves the workspace", () => {
		for (const workingDirectory of ["/etc", "../../etc", "..", "backend/../..", "\\\\server\\share", "C:/repo"]) {
			expect(commandDraftProblem(draft({ command: "go", workingDirectory })), workingDirectory).toBe(
				"workingDirectoryOutside",
			);
		}
	});

	it("accepts a working directory that stays inside it", () => {
		for (const workingDirectory of ["backend", "./backend", "backend/internal", "a/../b"]) {
			expect(commandDraftProblem(draft({ command: "go", workingDirectory })), workingDirectory).toBeUndefined();
		}
	});

	it("holds the timeout to the daemon's own range", () => {
		expect(commandDraftProblem(draft({ command: "go", timeoutSeconds: "0" }))).toBeUndefined();
		expect(commandDraftProblem(draft({ command: "go", timeoutSeconds: "3600" }))).toBeUndefined();
		expect(commandDraftProblem(draft({ command: "go", timeoutSeconds: "3601" }))).toBe("timeoutRange");
		expect(commandDraftProblem(draft({ command: "go", timeoutSeconds: "-1" }))).toBe("timeoutRange");
		expect(commandDraftProblem(draft({ command: "go", timeoutSeconds: "1.5" }))).toBe("timeoutRange");
		expect(commandDraftProblem(draft({ command: "go", timeoutSeconds: "soon" }))).toBe("timeoutRange");
	});

	it("requires a whole-number exit code", () => {
		expect(commandDraftProblem(draft({ command: "go", requiredExitCode: "1" }))).toBeUndefined();
		expect(commandDraftProblem(draft({ command: "go", requiredExitCode: "ok" }))).toBe("exitCodeInvalid");
		expect(commandDraftProblem(draft({ command: "go", requiredExitCode: "0.5" }))).toBe("exitCodeInvalid");
	});

	// The list of executables a verification may run is the daemon's policy
	// (workflow.ValidateVerifyCommand), enforced there and only there. A second
	// copy in the renderer would drift from the one that decides; a command the
	// form accepts and the daemon refuses comes back as a create error.
	it("does not second-guess which executables are allowed", () => {
		expect(commandDraftProblem(draft({ command: "bash", args: "-c 'go test'" }))).toBeUndefined();
	});
});

describe("verificationIsExecutable", () => {
	it("needs one stated command and no half-typed ones", () => {
		const good = draft({ command: "go", args: "test ./..." });
		expect(verificationIsExecutable([good])).toBe(true);
		// The trailing empty row the editor always offers must not block.
		expect(verificationIsExecutable([good, newCommandDraft()])).toBe(true);
		expect(verificationIsExecutable([good, draft({ args: "build" })])).toBe(false);
		expect(verificationIsExecutable([])).toBe(false);
	});
});

describe("buildVerificationPlan", () => {
	it("sends what was typed, with the optional fields omitted rather than zeroed", () => {
		expect(buildVerificationPlan([draft({ command: " go ", args: " test ./... " }), newCommandDraft()])).toEqual({
			commands: [{ command: "go", args: ["test", "./..."], requiredExitCode: 0, retrySafe: true }],
		});
	});

	it("carries every stated field through", () => {
		expect(
			buildVerificationPlan([
				draft({
					command: "go",
					args: "build ./...",
					workingDirectory: "backend",
					timeoutSeconds: "120",
					requiredExitCode: "1",
					retrySafe: false,
				}),
			]),
		).toEqual({
			commands: [
				{
					command: "go",
					args: ["build", "./..."],
					workingDirectory: "backend",
					timeoutSeconds: 120,
					requiredExitCode: 1,
					retrySafe: false,
				},
			],
		});
	});

	it("keeps the rows in the order they were entered", () => {
		const plan = buildVerificationPlan([
			draft({ command: "go", args: "test ./..." }),
			draft({ command: "go", args: "build ./..." }),
		]);
		expect(plan.commands?.map((c) => c.args?.[0])).toEqual(["test", "build"]);
	});
});

describe("parseAcceptanceCriteria", () => {
	it("is one per line, trimmed, with blank lines dropped", () => {
		expect(parseAcceptanceCriteria('  Farewell("") returns "Goodbye!"  \n\n  No unrelated files.\n')).toEqual([
			'Farewell("") returns "Goodbye!"',
			"No unrelated files.",
		]);
	});

	it("is empty for empty input, so the daemon's own criteria stay in place", () => {
		expect(parseAcceptanceCriteria("")).toEqual([]);
		expect(parseAcceptanceCriteria("\n \n")).toEqual([]);
	});
});
