import { apiErrorCode, apiErrorMessage } from "./api-client";

// Plain-English text for the project registration/clone/workflow error codes
// a non-technical operator can actually hit (checkpoint 8H.5 §10). The
// technical code is kept alongside for details/logs, never as the headline.
const PROJECT_ERROR_MESSAGES: Record<string, string> = {
	PATH_OUTSIDE_ALLOWED_ROOTS: "That path is outside the allowed project roots.",
	NOT_A_GIT_REPO: "That folder is not a Git repository.",
	PROJECT_UNBORN: "That repository has no commits yet.",
	PATH_ALREADY_REGISTERED: "A project at this path is already registered.",
	ID_ALREADY_REGISTERED: "A project with this id is already registered.",
	NO_ALLOWED_ROOTS_CONFIGURED: "No allowed project roots are configured and this server's home directory could not be resolved.",
	PATH_NOT_FOUND: "Folder not found under any allowed project root.",
	PATH_NOT_ABSOLUTE: "That folder could not be opened. Try browsing again from the top.",
	INVALID_GITHUB_REPO: 'Enter a GitHub repo as "owner/repo" or a github.com URL.',
	DESTINATION_ALREADY_EXISTS: "A folder already exists at that destination.",
	GITHUB_NOT_AUTHENTICATED: "GitHub CLI is not authenticated. Run `gh auth login` and try again.",
	GITHUB_CLONE_FAILED: "Cloning the repository failed.",
	INVALID_DESTINATION_NAME: "Destination name must contain only letters, numbers, '.', '_', or '-'.",
	PATH_REQUIRED: "Enter a path.",
	INVALID_PATH: "That path is invalid.",
};

export function describeProjectApiError(error: unknown): { message: string; code?: string } {
	const code = apiErrorCode(error);
	const message = (code && PROJECT_ERROR_MESSAGES[code]) || apiErrorMessage(error);
	return { message, code };
}
