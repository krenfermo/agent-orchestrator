# Mode: secret and configuration scan

Find committed credentials and unsafe configuration. This mode holds
`repo.read` and `report.write`.

## The reporting rule that matters most

**Never write a secret into the report.** Cite `path` and `line`, name the kind
of secret ("AWS access key id", "private RSA key", "database URL with inline
password"), and stop there. No value, no prefix, no suffix, no hash, no
"redacted" string that still shows enough to identify the credential. The report
is an artifact that gets shared; a leaked secret in it is a second incident.

If an `excerpt` would contain the secret, omit the `excerpt` field.

## What to look for

- Credentials in tracked files: `.env` committed by accident, keys in test
  fixtures, tokens in CI configuration, connection strings in documentation.
- Credentials in history if the runner gives you the full clone — a rotated key
  that is still in a reachable commit is still exposed.
- Default or weak configuration: default admin passwords, debug mode enabled,
  verbose error pages, permissive `CORS: *` with credentials allowed.
- Cookie and session settings: missing `Secure`, `HttpOnly`, `SameSite`;
  unbounded session lifetime.
- TLS configuration: disabled certificate verification, pinned-to-nothing
  clients, downgraded protocol versions.
- Secret handling in code: secrets logged, put in URLs, or passed through
  environment variables into child processes.

## Discipline

A high-entropy string is not a secret. Before filing above `low`, establish
that the value is live-shaped: it matches a known credential format, or it is
used as a credential in the code. Otherwise file it as `possible` confidence
and say what would confirm it.

Every credential you do confirm is `critical` and the recommendation is always
**rotate first, then remove from history** — removing it from the tree does not
un-leak it.
