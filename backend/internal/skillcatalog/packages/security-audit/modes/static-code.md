# Mode: static-code

Read the checkout and report code-level weaknesses. **Read only.** This mode
holds `repo.read` and `report.write` and nothing else: it may not run a scanner,
open a network connection, or write to the tree.

## Scope

Honour `scopePaths` exactly. Everything outside it belongs in
`coverage.skipped` with the reason `out-of-scope`.

## What to look for

- **Injection**: SQL/NoSQL built by string concatenation, shell commands built
  from request data, template rendering of untrusted input, `eval`-shaped calls.
- **Deserialization and parsing**: untrusted input into a deserializer that can
  instantiate arbitrary types; XML parsers with external entities enabled.
- **Path handling**: user-controlled path segments reaching the filesystem
  without normalisation and containment; archive extraction without a path check.
- **Unsafe defaults**: permissive fallbacks in error paths, a `default:` branch
  that grants rather than denies, feature flags that open access when unset.
- **Input validation**: boundaries that trust a client-supplied identifier,
  length, count, or content type.
- **Crypto misuse**: home-rolled crypto, ECB, static IVs, MD5/SHA-1 for
  authentication, predictable randomness for tokens.
- **Error handling that leaks**: stack traces, SQL text or internal paths in
  responses.

## Discipline

Trace each candidate to a reachable entry point before reporting it above
`low`. Code that looks dangerous but is unreachable from any input is a `low`
finding with the reachability stated, not a `high`.

If you want a third-party analyzer, say so in `notes`: running one needs
`process.exec`, which this mode does not hold. Do not shell out.
