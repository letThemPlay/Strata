# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/letThemPlay/Strata/security/advisories/new)
("Report a vulnerability" on the repository's Security tab). Do not open a
public issue.

Include what is affected (component, commit or version), how to reproduce
it, and the impact you expect. You will get an acknowledgement, and the fix
and advisory are coordinated with you before anything is published.

## Supported versions

Strata is in early development and has no releases yet. Fixes are made on
`main` only. Every capability is experimental (see the status table in
[ARCHITECTURE.md](ARCHITECTURE.md#current-status)); do not rely on Strata to protect
production systems yet.

## How dependencies are kept up to date

See [ADR-0022](ARCHITECTURE.md#adr-0022-dependency-updates-and-code-scanning):
Dependabot version updates, CodeQL, and govulncheck against the Go toolchain
pinned by `flake.lock`.
