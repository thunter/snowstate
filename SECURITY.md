# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately through
[GitHub's vulnerability reporting](https://github.com/thunter/snowstate/security/advisories/new).

Include what you found, how to reproduce it, and the version affected. This is a project run in spare time, so responses
are best effort, but reports are read and taken seriously.

## Supported versions

Only the latest release gets security fixes.

## What to keep in mind

Terraform state often contains secrets. Never attach a real state file, token, private key or log containing them to a
report or an issue. `docs/operator-guide.md` describes how to restrict access to state.

Project roles and token scopes are the isolation boundaries, so a way past either one is a security issue. So is anything
that writes a state body, token, password or `Authorization` header to a log.
