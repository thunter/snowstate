# Contributing

Thanks for helping. Please open an issue before starting anything large, so we can agree on the approach first.

## Setup

You need Go 1.27 or later, `golangci-lint`, `make` and `curl`.

```bash
make build
make lint test     # unit tests, no network
make e2e           # downloads Terraform and OpenTofu, then runs them against a memory store
```

`make integration` runs against real Snowflake. It needs the `SNOWSTATE_IT_*` variables in `dev_setup.md`, and skips itself
without them. Please run it for anything that changes Snowflake behaviour.

## Pull requests

- Branch from `main`. Direct pushes to `main` are blocked.
- Keep each pull request to one change, with a test for any new or fixed behaviour.
- `make lint test e2e` must pass. CI skips its long jobs when a pull request only changes `docs/`, Markdown files,
  `LICENSE` or `NOTICE`.
- Update `CHANGELOG.md` and any affected docs.

## Project rules

- `SPEC.md` is the source of truth. Record any deviation in `docs/decisions.md`.
- Keep `pkg/` free of frontend code. `TestImportBoundaries` enforces this.
- Never log state bodies, tokens, passwords or `Authorization` headers.

## Releases

Versions follow [semantic versioning](https://semver.org). For a stable release, a maintainer first renames the
`## Unreleased` section of `CHANGELOG.md` to `## 0.1.0 - <date>` in a pull request, adds a new empty `## Unreleased` above
it, and merges that. Then they tag the merged commit, and `.github/workflows/release.yml` does the rest:

```bash
git switch main && git pull
git tag v0.1.0
git push origin v0.1.0
```

The release fails before anything is published if the changelog has no section for that version.

A tag with a suffix, such as `v0.2.0-rc.1`, skips that check. It is published as a prerelease and does not move the major
tag.

## Reporting security problems

See `SECURITY.md`. Please do not open a public issue.
