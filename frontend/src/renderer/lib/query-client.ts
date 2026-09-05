import { QueryClient } from "@tanstack/react-query";

// React Query's own default retry count, kept explicit because the predicate
// below has to supply one.
const MAX_RETRIES = 3;

// A refused request does not become an allowed one by being asked again. Every
// retry of a 401 is a request the daemon has already answered, and with a
// refetchInterval behind it that is a permanent loop rather than a burst.
// api-client reports the same response to auth-store, which is what actually
// resolves the state; retrying only delays it.
export function isUnauthorized(error: unknown): boolean {
	// Read off the daemon's error envelope directly rather than through
	// api-client: this module is imported by every query, including in suites
	// that mock api-client down to the two methods they exercise.
	return typeof error === "object" && error !== null && (error as { code?: unknown }).code === "NOT_AUTHENTICATED";
}

export const queryClient = new QueryClient({
	defaultOptions: {
		queries: {
			staleTime: 10_000,
			refetchOnWindowFocus: false,
			retry: (failureCount, error) => !isUnauthorized(error) && failureCount < MAX_RETRIES,
		},
	},
});
