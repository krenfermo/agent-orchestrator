// Package plane implements ports.WorkItems against the Plane REST API v1.
//
// THE CONTRACT THIS IS WRITTEN AGAINST, and where each part was verified.
// Nothing here is inferred from a plausible-looking URL: every path, header
// and field name below was read either from Plane's published API reference or
// from the server source that produces it (makeplane/plane,
// apps/api/plane/api/).
//
//	Base URL      https://api.plane.so for Plane Cloud; a self-hosted
//	              installation's own origin otherwise. AO always appends
//	              /api/v1 itself, so an operator configures the ORIGIN and
//	              cannot get the version prefix wrong.
//
//	Auth          "X-API-Key: <token>". A personal access token, created in
//	              Plane under Profile Settings → Personal Access Tokens.
//	              Plane also accepts OAuth bearer tokens; AO implements the
//	              API-key path only, because that is the one an unattended
//	              daemon can hold without a browser round trip.
//
//	Scoping       Every path is workspace-scoped by SLUG, then project-scoped
//	              by UUID:
//	                GET  /api/v1/workspaces/{slug}/projects/
//	                GET  /api/v1/workspaces/{slug}/projects/{project}/states/
//	                GET  /api/v1/workspaces/{slug}/projects/{project}/work-items/
//	                POST /api/v1/workspaces/{slug}/projects/{project}/work-items/
//	                GET  /api/v1/workspaces/{slug}/projects/{project}/work-items/{id}/
//	                PATCH .../work-items/{id}/
//	                POST .../work-items/{id}/comments/
//	                GET  /api/v1/workspaces/{slug}/work-items/{PROJ}-{123}/
//	              The trailing slashes are required: Plane is a Django app and
//	              a request without one is redirected, which silently drops the
//	              body of a POST.
//
//	Identifiers   A work item has BOTH a UUID (`id`, what every path takes)
//	              and a human key built from the project's `identifier` prefix
//	              and the item's `sequence_id` ("PROJ-123"). The by-identifier
//	              route above is what makes a pasted reference resolvable.
//
//	Pagination    Cursor-based. Request `per_page` (max 100) and `cursor`;
//	              the envelope carries `results`, `next_cursor`,
//	              `next_page_results`, `count`, `total_pages`. AO follows
//	              `next_cursor` while `next_page_results` is true, under a hard
//	              page ceiling.
//
//	Rate limit    60 requests per minute per API key on Plane Cloud, reported
//	              in `X-RateLimit-Remaining` and `X-RateLimit-Reset` (UTC epoch
//	              seconds). A 429 is retryable and carries the reset hint.
//
//	States        A project defines its own states, each belonging to one of
//	              six groups: backlog, unstarted, started, completed,
//	              cancelled, triage (StateGroup in apps/api/plane/db/models/
//	              state.py). AO writes by GROUP and resolves a concrete state
//	              within it, which is what lets it drive a workspace whose
//	              state names nobody told it.
//
//	External ids  Work items and comments both carry `external_source` and
//	              `external_id`. Plane indexes on the pair: a POST that would
//	              duplicate one returns 409 with the existing item's id in the
//	              body, and there is a PUT upsert keyed on it. AO writes its
//	              own scope:id into these, which is what makes creating an item
//	              for a run safe to retry without AO remembering anything.
//
// WHAT THIS ADAPTER DELIBERATELY DOES NOT DO. It does not create labels, does
// not add or remove assignees, does not touch cycles, modules or estimates,
// and does not delete anything. Every one of those is somebody else's planning
// data, and an integration that reshapes it is one an operator has to supervise
// rather than switch on. The write surface is exactly: create an item, move its
// state, add a comment.
//
// NO SECRET EVER REACHES A LOG. The token is held in the client, sent as a
// header, and is not included in any error this package constructs: error
// messages are built from the provider's own response body, truncated, and the
// request URL is recorded without its query string.
package plane
