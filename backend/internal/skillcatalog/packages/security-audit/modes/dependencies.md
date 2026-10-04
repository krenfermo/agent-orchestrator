# Mode: dependency review

Inventory the project's declared dependencies and report what the manifests and
lockfiles establish **on their own**. This mode holds `repo.read`, `deps.read`
and `report.write`. It is **offline**: it holds no `net.egress`, consults no
advisory database, and therefore never claims a vulnerability.

AO runs it as the deterministic tool `ao.dependency-scan/v1` in the container
runner. Only manifests and lockfiles are staged; the scan never receives source
code.

## Scope

npm (`package.json` with `package-lock.json`, `npm-shrinkwrap.json`,
`yarn.lock` or `pnpm-lock.yaml`), Go (`go.mod`, `go.sum`), pip
(`requirements*.txt`) and Cargo (`Cargo.toml`). Other manifests that are present
(Gemfile, pyproject.toml, pom.xml, ...) are listed as **unparsed**, not ignored.

## What it reports

- An inventory: ecosystem, name, the version spec as written, where.
- `DEP-001` a dependency installed from a git or URL source instead of a registry.
- `DEP-002` a version that is not pinned (`*`, `latest`, none).
- `DEP-003` a manifest that declares dependencies with no lockfile beside it.
- `DEP-004` a registry or package source reached over plain HTTP.

Each is a fact read off a file, reported as `confirmed`. None is a statement
about a vulnerability.

## What it does not do

Known-vulnerable versions need advisory data, and advisory data needs either a
local database AO does not ship or `net.egress`, which stays refused. Until one
exists, "no findings" from this mode means "no hygiene problem these rules
recognise", never "no vulnerable dependency".

## Reporting rule

A dependency URL can carry a credential. The inventory removes the userinfo of
every URL before it is printed, and AO redacts again before storing.
