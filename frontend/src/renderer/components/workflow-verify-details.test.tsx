import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowVerifyDetails } from "./workflow-verify-details";

describe("WorkflowVerifyDetails", () => {
	it("shows checks, exit code, duration, fingerprint, and failure", () => {
		render(<WorkflowVerifyDetails result={{version:"v1",targetKey:"target",reviewedFingerprint:"reviewed",preFingerprint:"pre",postFingerprint:"post",passed:false,errorClass:"verify_command_failed",checks:[{kind:"command",label:"go test ./...",passed:false,exitCode:1,durationMs:42,failureReason:"exit code 1"}]}} />);
		expect(screen.getByText("FAIL")).toBeInTheDocument();
		expect(screen.getByText("post")).toBeInTheDocument();
		expect(screen.getByText("verify_command_failed")).toBeInTheDocument();
		expect(screen.getByText(/FAIL · go test/)).toHaveTextContent("exit 1");
		expect(screen.getByText(/FAIL · go test/)).toHaveTextContent("42 ms");
	});

	it("shows the Checkpoint 8I verify scope decision, with package dir when narrowed", () => {
		render(
			<WorkflowVerifyDetails
				result={{
					version: "v1",
					targetKey: "target",
					reviewedFingerprint: "reviewed",
					preFingerprint: "pre",
					postFingerprint: "post",
					passed: true,
					checks: [],
					scope: { policyVersion: "v1", scope: "targeted", packageDir: "internal/foo", reasons: ["single_go_package_changed"], changedFiles: ["internal/foo/bar.go"] },
				}}
			/>,
		);
		expect(screen.getByText("targeted (internal/foo)")).toBeInTheDocument();
	});
});
