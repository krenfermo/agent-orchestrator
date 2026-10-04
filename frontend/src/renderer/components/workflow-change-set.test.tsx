import { render, screen } from "@testing-library/react";
import { I18nextProvider } from "react-i18next";
import { describe, expect, it } from "vitest";
import type { components } from "../../api/schema";
import { createAppI18n } from "../i18n/instance";
import { WorkflowChangeSet } from "./workflow-change-set";

type ReviewRiskFacts = components["schemas"]["WorkflowReviewRiskFacts"];

/**
 * Phase D scenario 5, as a test: a file the worker wrote that Git ignores must
 * produce a clear notice, never the appearance that no work was done.
 *
 * The distinction under test is `taskChangeSet.Proven()`: AO separates "I
 * established that nothing changed" from "I could not establish what changed",
 * and the product used to render both as the same empty review block.
 */
function facts(overrides: Partial<ReviewRiskFacts> = {}): ReviewRiskFacts {
	return {
		acceptanceCriteriaEmpty: false,
		changedFileCount: 0,
		changedFilePaths: [],
		hasExactContentCheckForSoleChangedFile: false,
		priorWorkProviderAttempts: 0,
		verifyCommandCount: 1,
		verifyFileCheckCount: 0,
		...overrides,
	};
}

function renderFacts(value: ReviewRiskFacts | undefined, locale: "en" | "es" = "en") {
	return render(
		<I18nextProvider i18n={createAppI18n(locale)}>
			<WorkflowChangeSet facts={value} />
		</I18nextProvider>,
	);
}

describe("WorkflowChangeSet", () => {
	it("renders nothing when the step carries no risk facts", () => {
		const { container } = renderFacts(undefined);
		expect(container).toBeEmptyDOMElement();
	});

	// The headline defect. An unprovable change set is a warning, and it carries
	// the daemon's own sentence about the cause.
	it("warns, and passes on AO's reason, when the change set could not be established", () => {
		renderFacts(
			facts({
				changedFilesUnprovable: true,
				changedFilesSource: "unprovable",
				unprovableChangeSetReason:
					"the work step recorded no base commit, so AO cannot tell committed work from work that was never done",
			}),
		);
		expect(screen.getByText("AO could not establish what this work changed")).toBeInTheDocument();
		expect(
			screen.getByText(
				"the work step recorded no base commit, so AO cannot tell committed work from work that was never done",
			),
		).toBeInTheDocument();
	});

	// The other side of the same distinction: a genuinely empty, PROVEN set must
	// not borrow the warning wording.
	it("distinguishes a proven empty change set from an unprovable one", () => {
		renderFacts(facts({ changedFileCount: 0, changedFilesSource: "worktree_only" }));
		expect(screen.getByText("AO established that no files changed")).toBeInTheDocument();
		expect(screen.queryByText("AO could not establish what this work changed")).not.toBeInTheDocument();
	});

	it("reports the count, the committed subset and the diff range it was taken from", () => {
		renderFacts(
			facts({
				changedFileCount: 3,
				committedChangedFileCount: 2,
				changedFilesSource: "base_diff_and_worktree",
				changeSetBaseSha: "aaaaaaaaaaaabbbbbbbb",
				changeSetHeadSha: "ccccccccccccdddddddd",
				changedFilePaths: ["src/a.ts", "src/b.ts", "src/c.ts"],
			}),
		);
		expect(screen.getByText("3 files changed")).toBeInTheDocument();
		expect(screen.getByText("2 of 3 came from the branch history")).toBeInTheDocument();
		expect(screen.getByText("aaaaaaaaaaaa..cccccccccccc")).toBeInTheDocument();
		expect(screen.getByText("src/b.ts")).toBeInTheDocument();
	});

	it("localizes the unprovable warning", () => {
		renderFacts(facts({ changedFilesUnprovable: true, changedFilesSource: "unprovable" }), "es");
		expect(screen.getByText("AO no pudo establecer qué cambió este trabajo")).toBeInTheDocument();
	});
});
