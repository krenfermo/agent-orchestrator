import { createBrowserHistory, createHashHistory, createRouter } from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import { routeTree } from "./routeTree.gen";
import { isDesktopMode } from "./lib/platform-adapter";

// Electron keeps hash history because its app:// protocol owns renderer file
// resolution. Headless web uses browser history so copy/paste and hard refresh
// work at ordinary paths; the Go server provides the corresponding SPA fallback.
export function createAppRouter(queryClient: QueryClient) {
	return createRouter({
		history: isDesktopMode() ? createHashHistory() : createBrowserHistory(),
		routeTree,
		context: { queryClient },
		defaultPreload: "intent",
		// Always re-run loaders when a route is preloaded or visited so React
		// Query's cache is the single source of truth for staleness.
		defaultPreloadStaleTime: 0,
		scrollRestoration: true,
	});
}
