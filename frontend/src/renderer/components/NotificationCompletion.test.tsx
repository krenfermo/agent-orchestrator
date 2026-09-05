import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { NotificationDTO, NotificationListStatus } from "../lib/notifications";
import { NotificationCenter } from "./NotificationCenter";
import { TooltipProvider } from "./ui/tooltip";

const { fetchNextPageMock, markAllMock, navigateMock, notificationQueryMock, paramsMock, restoreSessionMock, workspaceQueryMock } =
	vi.hoisted(() => ({
		fetchNextPageMock: vi.fn(),
		markAllMock: vi.fn(),
		navigateMock: vi.fn(),
		notificationQueryMock: vi.fn(),
		paramsMock: vi.fn(),
		restoreSessionMock: vi.fn(),
		workspaceQueryMock: vi.fn(),
	}));

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => navigateMock, useParams: () => paramsMock() }));

vi.mock("../hooks/useNotificationsQuery", () => ({
	useMarkAllNotificationsReadMutation: () => ({ isPending: false, mutateAsync: markAllMock }),
	useNotificationsQuery: (status: NotificationListStatus, enabled?: boolean) => notificationQueryMock(status, enabled),
}));

vi.mock("../hooks/useRestoreSession", () => ({ useRestoreSession: () => restoreSessionMock }));

vi.mock("../hooks/useWorkspaceQuery", () => ({
	useWorkspaceQuery: () => workspaceQueryMock(),
	workspaceQueryKey: ["workspaces"],
}));

const taskCompleted: NotificationDTO = {
	id: "ntf_task",
	sessionId: "sess-1",
	projectId: "proj-1",
	prUrl: "",
	type: "task_completed",
	title: "Checkout flow finished",
	body: "The task reported that it finished the work it was given.",
	status: "unread",
	severity: "info",
	createdAt: "2026-08-22T10:00:00Z",
	target: { kind: "session", sessionId: "sess-1" },
};

// A run-level notification: no session at all, because a workflow run is not a
// session and a master run coordinates many of them.
const workflowCompleted: NotificationDTO = {
	id: "ntf_workflow",
	sessionId: "",
	projectId: "proj-1",
	prUrl: "",
	workflowRunId: "wf-1",
	type: "workflow_completed",
	title: "Ship the thing finished",
	body: "Every task in this workflow run completed.",
	status: "unread",
	severity: "info",
	createdAt: "2026-08-22T09:00:00Z",
	target: { kind: "workflow", sessionId: "", workflowRunId: "wf-1" },
};

const completions = [taskCompleted, workflowCompleted];

function queryResult() {
	return {
		data: {
			pageParams: [""],
			pages: [{ notifications: completions, unreadCount: completions.length, unresolvedCount: 0 }],
		},
		fetchNextPage: fetchNextPageMock,
		hasNextPage: false,
		isError: false,
		isFetchNextPageError: false,
		isFetchingNextPage: false,
		isLoading: false,
	};
}

function renderCenter() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<NotificationCenter />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

async function openPanel() {
	await userEvent.click(screen.getByRole("button", { name: /unread notifications/ }));
	await screen.findByRole("dialog", { name: "Notifications" });
}

beforeEach(() => {
	paramsMock.mockReset().mockReturnValue({});
	fetchNextPageMock.mockReset().mockResolvedValue(undefined);
	markAllMock.mockReset().mockResolvedValue(0);
	navigateMock.mockReset();
	restoreSessionMock.mockReset().mockResolvedValue({ status: "success" });
	workspaceQueryMock.mockReset().mockReturnValue({
		data: [
			{
				id: "proj-1",
				name: "acme/app",
				sessions: [{ id: "sess-1", isTerminated: false, status: "idle", title: "Checkout flow" }],
			},
		],
		isError: false,
		isPending: false,
		isSuccess: true,
		refetch: vi.fn(),
	});
	notificationQueryMock.mockReset().mockImplementation(() => queryResult());
});

describe("completion notifications in the bell", () => {
	it("shows a finished task and a finished workflow", async () => {
		renderCenter();
		await openPanel();

		expect(screen.getByText("Checkout flow finished")).toBeInTheDocument();
		expect(screen.getByText("Ship the thing finished")).toBeInTheDocument();
	});

	it("counts completions in the unread badge", () => {
		renderCenter();
		expect(screen.getByRole("button", { name: /2 unread notifications/ })).toBeInTheDocument();
	});

	// A finished task still has a live session worth opening; a finished run has
	// no session, so its row must not pretend to be a link to one.
	it("opens the session behind a finished task but not behind a finished run", async () => {
		renderCenter();
		await openPanel();

		const rows = screen.getAllByRole("listitem");
		const taskRow = rows.find((row) => row.textContent?.includes("Checkout flow finished"));
		const workflowRow = rows.find((row) => row.textContent?.includes("Ship the thing finished"));

		expect(within(taskRow as HTMLElement).getByRole("button")).toBeInTheDocument();
		expect(within(workflowRow as HTMLElement).queryByRole("button")).toBeNull();

		await userEvent.click(within(taskRow as HTMLElement).getByRole("button"));
		expect(navigateMock).toHaveBeenCalled();
	});
});
