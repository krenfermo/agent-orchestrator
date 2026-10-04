# Mode: authentication and authorization review

Review who can reach what. This mode holds `repo.read` and `report.write`.

## Authentication

- How is identity established, and where is that check enforced? A check in a
  UI route that the API route does not repeat is not a check.
- Session and token handling: issuance, expiry, revocation, rotation, and what
  happens to an existing session when an account is disabled or its role drops.
- Password and credential storage: algorithm, cost, and whether comparison is
  constant-time.
- Multi-step flows (OIDC, magic links, device codes): state parameter,
  redirect-URI validation, replay of the authorization code.

## Authorization and RBAC

- Find the single place a permission decision is made. If there is more than
  one, they will disagree; that divergence is the finding.
- For every route, handler or resolver: which permission does it require, and
  is that requirement enforced on the **server**, before any data is read?
- Are permissions total? A role table that leaves a permission undecided
  defaults to something, and the default is usually "allow" by accident.
- Privilege escalation paths: can a user grant themselves a role, add
  themselves to a team, or change the owner of a resource they merely hold?

## IDOR and tenant isolation

This is the highest-yield part of the mode. For every handler that takes an
identifier from the request:

1. Is the identifier used to fetch a row **before** the caller's right to that
   row is checked?
2. Is the ownership/tenancy predicate part of the query, or applied afterwards
   in code that an early return can skip?
3. Do list endpoints filter by tenant, or filter in the presentation layer?
4. Are identifiers guessable (sequential integers) — which turns a missing
   check into mass exposure rather than a targeted one?

Report each as its own finding, with the exact handler and the query that is
missing the predicate.

## Discipline

Trace one concrete request path per finding, from entry point to data access.
A finding that names a pattern without a path is a `possible`, not a
`confirmed`.
