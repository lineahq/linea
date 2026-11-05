# Contributing to Linea

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).
Report security issues privately as described in [SECURITY.md](SECURITY.md).

## Build and test

[AGENTS.md](AGENTS.md) describes the repository layout and the rules a change
must follow. [docs/internals/development.md](docs/internals/development.md) lists
every build, test and Docker command.

In short, the repository holds several Go modules, so run the tools per module:

```sh
go build ./... && go vet ./... && go test ./...      # root module
go -C collector/livecapture test ./...               # a nested module
pnpm --dir web install && pnpm --dir web test        # web client
```

Update the documentation a change touches in the same commit.

## Sign your commits

Linea uses the [Developer Certificate of Origin](https://developercertificate.org)
(DCO) instead of a contributor license agreement. By adding a `Signed-off-by`
line you certify that you wrote the change or have the right to submit it under
the project's license (Apache-2.0).

Sign each commit with `git commit -s`, which appends:

```
Signed-off-by: Your Name <you@example.com>
```

The name and email must match the commit author. Pseudonymous sign-offs are
accepted as long as the identity is consistent across your commits and
reachable, for example a GitHub noreply address
(`12345+handle@users.noreply.github.com`).

To sign commits you already made: `git rebase --signoff <base>`.

## Versions and releases

The git tag is the version. Linea follows [Semantic Versioning](https://semver.org);
before 1.0 a minor release may break APIs, report schemas and persisted
formats, and its changelog says how. Every shipped part (CLI, worker,
builder, team server and fleet images, Helm chart) carries the same version.

To release, a maintainer moves the `## [Unreleased]` entries in
[CHANGELOG.md](CHANGELOG.md) under a new `## [X.Y.Z]` heading, sets `X.Y.Z`
as the chart's `version` and `appVersion` and as the default of
`LINEA_VERSION`, `FLEET_VERSION` and `CAPTURE_VERSION` in the Compose files
and as `LINEA_VERSION` in `examples/ci/`, and as the root `go.mod`'s
version of `github.com/lineahq/linea/collector/livecapture` (the release
workflow refuses a tag they don't match), commits, and pushes
the tag `vX.Y.Z`. The release workflow builds everything with that
version, pushes the `linux/amd64` and `linux/arm64` images to `ghcr.io/lineahq/`, tags the
`collector/livecapture` and `collector/provider` Go modules, and publishes
the CLI archives and the chart on the GitHub release. Every other build reports
`0.0.0-dev`; to stamp one yourself, pass
`-ldflags "-X github.com/lineahq/linea/internal/debugger/release.version=X.Y.Z"`
to `go build`, or `--build-arg VERSION=X.Y.Z` to `docker build`.

## Pull requests

Every pull request must pass CI, which builds the product images, and the DCO check,
which fails if any commit lacks a `Signed-off-by` line matching its author. CI
runs no tests, so run the checks your change touches locally and list the
validation commands you ran in the pull request; the template asks for them.

Add a line under `## Unreleased` in [CHANGELOG.md](CHANGELOG.md) for a
change users will notice.
