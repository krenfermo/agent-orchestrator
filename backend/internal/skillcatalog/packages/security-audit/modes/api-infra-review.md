# Mode: API and infrastructure review

Review the API surface and the deployment posture as described by the
repository's own files. This mode holds `repo.read` and `report.write`: it reads
configuration, it does not connect to anything.

## API surface

- Every route and its authentication requirement, in one list. Anything
  unauthenticated is a finding candidate until justified.
- Rate limiting and abuse controls on authentication, password reset, invite,
  and any expensive query.
- Mass assignment: request bodies bound straight onto persisted models.
- Response shaping: does an object serializer leak fields the caller should not
  see (internal ids, other users' data, password hashes, tokens)?
- Error disclosure: distinct messages for "no such user" and "wrong password",
  stack traces in production responses, request ids that encode internals.
- File upload: type and size limits, storage location, and whether uploaded
  content can be served back from the application's own origin.

## Infrastructure as described in the repo

- Container images: base image and its currency, `USER` (running as root is a
  finding), build secrets in layers, `COPY . .` pulling in `.env` or `.git`.
- Compose and deployment manifests: ports published to `0.0.0.0` that only need
  loopback, privileged containers, host mounts, missing resource limits.
- CI configuration: secrets exposed to pull-request builds from forks,
  third-party actions pinned to a moving tag rather than a digest, artifacts
  that carry credentials.
- Network posture: what is intended to be public, and does the configuration
  match that intent?

## Discipline

Distinguish "this file says X" from "production does X". You are reading
configuration, not observing a system. Say which, in `evidence.summary`, and set
`confidence` accordingly.
