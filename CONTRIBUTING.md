# Contributing to terraform-provider-muster

Thank you for helping. This guide covers how work is proposed, how a pull request is built and what it must prove.
[`AGENTS.md`](AGENTS.md) is the detailed guide for everyone who changes the code, people and coding agents alike; read
it before your first pull request.

## Licence of contributions

The provider is under the [MIT licence](LICENSE), and contributions are accepted under the same licence: by opening a
pull request you agree that your contribution is licensed under MIT. There is no Contributor License Agreement.

## Proposing work

Open an issue for a bug or a change you would like to see. Planned work is described as story contracts in Muster's
repository, in [`design/stories/`](https://github.com/muster-io/muster/tree/master/design/stories), with
`repository: muster-io/terraform-provider-muster` in their frontmatter; their issues, titled `S-NNN: <title>`, live in
[muster-io/muster](https://github.com/muster-io/muster/issues). A change to Muster's API belongs in Muster's repository:
[`api/openapi.yaml`](api/openapi.yaml) here is only its copy.

## Development setup

You need Go 1.27, GNU make, curl and git; `make docs` also needs Terraform, and the acceptance tests need Linux with
Docker, jq and Terraform or OpenTofu. The Makefile builds its pinned tools (golangci-lint, go-licenses, govulncheck,
oapi-codegen, tfplugindocs) into `bin/tools/` on first use; after switching Go releases, `make clean` rebuilds them.

The Makefile is the only entry point; `make help` lists the targets:

| Target | Does |
|---|---|
| `make fmt` | formats the code (gofmt and goimports through golangci-lint) |
| `make lint` | checks the licence headers, `go.mod` and `go.sum`, and the code with golangci-lint |
| `make test` | runs the unit tests with the race detector, and the coverage gate: 80 % of `internal/`, the generated client excluded |
| `make testacc` | runs the acceptance tests against `muster dev`: the one `MUSTER_ENDPOINT` names, or one that `test/muster-dev.sh` starts and stops |
| `make generate` | generates the client in `internal/client/gen/` from `api/openapi.yaml` with oapi-codegen |
| `make generate-check` | regenerates and fails when the client was not current |
| `make spec-sync` | pins the copy of Muster's specification: `VERSION=x.y.z` for a release, `COMMIT=<sha>` for a commit, or Muster's `master`; then runs `make generate` |
| `make spec-check` | compares `api/openapi.yaml` byte for byte with the specification of its pin and prints the difference |
| `make docs` | generates `docs/` with tfplugindocs from the schema, `templates/` and `examples/` |
| `make docs-check` | regenerates and fails when `docs/` was not current |
| `make licenses` | fails on a dependency whose licence is not allowed |
| `make vulncheck` | finds vulnerable Go code that is called (govulncheck) |
| `make build` | builds the provider into `bin/terraform-provider-muster` |
| `make ci` | runs `lint`, `generate-check`, `spec-check`, `docs-check`, `licenses` and `test` |
| `make clean` | removes `bin/` and `dist/` |

In CI, the `ci` workflow runs the jobs `lint`, `test`, `generate`, `spec`, `docs`, `licenses`, `vulncheck` and
`goreleaser` on every pull request, every push to `master`, daily and by hand; its job `ci` passes only when all of them
passed. The `acceptance` workflow runs the acceptance tests on Terraform and OpenTofu, and its jobs `acceptance` and
`old-cli` join `ci` among the checks the ruleset of `master` requires. The `codeql` and `scorecard` workflows analyse
the code and the repository's practices.

## Branches, commits and pull requests

- Branch from `master`; a story's branch is `s-NNN-<slug>`, with the slug of its story file name.
- Commit messages are [conventional commits](https://www.conventionalcommits.org/) on one line:
  `<type>(<scope>): <description>`, with `feat`, `fix`, `docs`, `build`, `ci`, `test`, `refactor`, `perf` or `chore`.
  Say what changed; leave out story and phase IDs. `build` means a dependency change, the type Renovate uses: it shows
  under "Dependencies" in the changelog and starts a release. Work on the build system or the tooling uses `chore` or
  `ci`.
- No AI attribution: no `Co-Authored-By` trailers for tools and no "Generated with" lines, in commits or pull requests.
- Pull requests are squash-merged, so the title becomes the commit message, and release-please writes the changelog
  from it.
- The [pull request template](.github/pull_request_template.md) asks for the issue in Muster's repository
  (`Part of muster-io/muster#<issue>`: a pull request here does not close it, so it is closed by hand once the pull
  request merges), the story ID, a table with every acceptance statement and its evidence, the output of the story's
  `verify`, every change to the story file with its reason, the dependencies added and the maintainer's steps.

## Licence header

Every Go file, workflow, YAML file, the Renovate configuration, shell script and the Makefile start with these two
lines in their own comment syntax:

```go
// SPDX-License-Identifier: MIT
// Copyright The Muster Authors
```

Markdown, JSON, `go.mod`, `go.sum`, the Terraform examples (copied into the documentation as they are), generated
files (marked `Code generated … DO NOT EDIT.`) and `api/openapi.yaml`, which states its licence in `info.license`, are
exempt. `make lint` names every file that lacks the header.

## Dependencies and third-party code

- The provider may depend only on code under MIT, MIT-0, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, 0BSD, Unlicense,
  CC0-1.0 or unmodified MPL-2.0 (HashiCorp's plugin libraries); `make licenses` checks it. Build tools may use any
  OSI-approved licence.
- A pull request that adds a dependency states its **licence**, its **latest release date** and its **maintenance
  status**.
- Never copy code from copyleft projects — Muster included, which is under the AGPL — into this repository; read them
  for behaviour only.

## Releases

release-please keeps a release pull request open, with the changelog written from the conventional commits on
`master`. Merging it creates the release and its `vX.Y.Z` tag, which starts `release.yml`: GoReleaser builds the zips,
signs `SHA256SUMS` with the project's GPG key and adds the Registry manifest, which the Terraform Registry and the
OpenTofu Registry read. Versions stay `0.x`, independent of Muster's.
