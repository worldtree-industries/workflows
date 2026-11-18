# workflows

Shared reusable GitHub Actions workflows for worldtree-industries Go projects.

Vocabulary (Project, Deployable, Tool, Kind, Manifest) follows
[ivaldi's GLOSSARY.md](https://github.com/worldtree-industries/ivaldi/blob/main/GLOSSARY.md).

## `.github/workflows/go.yml`: the Go Project workflow

The reusable workflow every Ivaldi Project's CI calls. Ivaldi generates the
caller; a Project's `.github/workflows/ci.yml` is:

```yaml
name: ci

on:
  pull_request:
  push:
    branches: [main]
    tags: ['v*.*.*']

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  go:
    uses: worldtree-industries/workflows/.github/workflows/go.yml@<commit-sha> # vX.Y.Z
    secrets:
      HARBOR_USERNAME: ${{ secrets.HARBOR_USERNAME }}
      HARBOR_PASSWORD: ${{ secrets.HARBOR_PASSWORD }}
```

### Pinning by SHA

Always call the workflow at a full commit SHA, with the release it
corresponds to as a comment, so that moving a tag can't change what runs:

```sh
gh api repos/worldtree-industries/workflows/commits/v1.0.0 --jq .sha
```

The workflow builds its own commands (`cmd/affected`, `cmd/plan`) from the
same commit it was called at, so the pin covers them too.

### Inputs and secrets

| Secret            | Required | Meaning |
|-------------------|----------|---------|
| `HARBOR_USERNAME` | yes      | Harbor robot account with push rights on `library`. An org-level secret; pass it by name. |
| `HARBOR_PASSWORD` | yes      | Its password. An org-level secret; pass it by name. |

Pass only these two, by name; never `secrets: inherit`. The workflow requests
no permissions beyond `contents: read`, so the caller needs nothing more.

| Input       | Default       | Meaning |
|-------------|---------------|---------|
| `platforms` | `linux/amd64` | Comma-separated platforms to build every Deployable's image for, as `os/arch[/variant]` with no spaces, for example `linux/amd64,linux/arm64`. With more than one, each image is a multi-platform index under its one tag. Empty means `linux/amd64`. |
| `fixture`   | `''`          | Internal, for this repository's self-test: a directory of the caller's repository to use as the Project. Forces a build-only run. Projects must not set it. |

To build multi-platform images, add `with:` to the caller's job:

```yaml
jobs:
  go:
    uses: worldtree-industries/workflows/.github/workflows/go.yml@<commit-sha> # vX.Y.Z
    with:
      platforms: linux/amd64,linux/arm64
    secrets:
      HARBOR_USERNAME: ${{ secrets.HARBOR_USERNAME }}
      HARBOR_PASSWORD: ${{ secrets.HARBOR_PASSWORD }}
```

ko cross-compiles, so ko Deployables build for any platform Go and their
`base_image` support. `build: dockerfile` Deployables are built with buildx
for the same platforms, but no emulation (QEMU) is set up, so for any
platform other than the runner's `linux/amd64` the Dockerfile must
cross-compile: run its build stage `FROM --platform=$BUILDPLATFORM …` and
build with `GOOS=$TARGETOS GOARCH=$TARGETARCH` (see
`testdata/fixture/cmd/boxed/Dockerfile`). A Dockerfile that runs commands in
a target-platform stage fails for non-native platforms.

### Jobs

- **lint**: golangci-lint v2.14.0 with the Project's own config.
- **test**: `go test -race ./...`, with Go from the Project's `go.mod`.
- **affected**: runs `plan` and `affected` (below) to decide what to build.
- **images**: one job per affected Deployable, for the `platforms` input
  (default `linux/amd64`), as
  `registry.werewolves.fyi/library/<name>`. Deployables with
  `build: dockerfile` are built with `docker build` from
  `cmd/<name>/Dockerfile` with the Project root as context; all others with
  ko (`ko build ./cmd/<name>`) on their `base_image`. Runs only after lint and
  test pass, and is skipped when nothing is affected.

| Event                           | Compared against                | Image tag      | Pushed |
|---------------------------------|---------------------------------|----------------|--------|
| push of a `vX.Y.Z` tag          | the highest lower `vX.Y.Z` tag (none: every Deployable) | `vX.Y.Z` | yes |
| push to `main`                  | the branch's previous tip (`github.event.before`; its parent after a force push) | `sha-<short>` | yes |
| pull request                    | the pull request's base commit  | `sha-<short>`  | no: never logs in |
| anything else                   | nothing: every Deployable       | `sha-<short>`  | no     |

`<short>` is the first 7 hex digits of the commit. Unaffected Deployables
aren't rebuilt or retagged.

### Third-party actions

Besides GitHub-owned `actions/*` (`actions/checkout`, `actions/setup-go`),
the workflow uses exactly these actions, each pinned by commit SHA. A
Project's allowed-actions setting must allow them:

- `golangci/golangci-lint-action`
- `docker/login-action`
- `docker/setup-buildx-action`
- `docker/build-push-action`

ko is installed with `go install github.com/google/ko@v0.19.1`, not an action.

### Self-test

`.github/workflows/self-test.yml` calls the workflow on every pull request and
push to `main` with `fixture: testdata/fixture`, a Project with one ko
Deployable, one `build: dockerfile` Deployable and one Tool, exercising the
build-only path. It calls it twice: once with the default `platforms`, and
once with `platforms: linux/amd64,linux/arm64`.

## `cmd/plan`: what the images job does

Prints, as GitHub Actions step outputs, what the images job compares against,
the image tag and whether to push, following the table above.

```sh
go run ./cmd/plan -repo <dir> -event <github.event_name> -ref <github.ref> \
  -sha <github.sha> [-before <github.event.before>] [-pr-base <base sha>]
```

On success it exits 0 and prints three lines:

```
base=v1.0.0
tag=v1.1.0
push=true
```

`base` is empty for a first release (pass it to `affected -base` as is).
`-sha` is required. Errors print to stderr and exit 1 (git failure) or 2 (bad
flags).

## `cmd/affected`: affected Deployables

Prints, as JSON, the Deployables of a Project that need building between two
git refs. The shared Go workflow runs it to decide which images to build.

```sh
go run github.com/worldtree-industries/workflows/cmd/affected@<sha> \
  -repo <dir> -base <ref> -head <ref>
```

| Flag    | Default | Meaning |
|---------|---------|---------|
| `-repo` | `.`     | The Project's git repository. Only its history is read: the working tree may be at any commit. |
| `-base` | empty   | The previous release (tag or commit). Empty, or the all-zero SHA GitHub reports for a newly created ref, means first release: every Deployable is affected. |
| `-head` | `HEAD`  | The commit being built. Its `.forge.yaml` decides which Deployables exist. |

On success it exits 0 and prints one line: a JSON array sorted by name, `[]`
when nothing is affected. Tools (`kind: tool`) never appear.

```json
[{"name":"api","build":"ko","base_image":"gcr.io/distroless/static-debian12:nonroot"}]
```

- `build` is `ko` or `dockerfile`.
- `base_image` is the entry's `base_image`, or
  `gcr.io/distroless/static-debian12:nonroot` when unset (it's reported for
  `dockerfile` builds too, but they ignore it).

On error it prints a message to stderr, nothing to stdout, and exits 1
(bad ref, missing or invalid `.forge.yaml` at head, `git`/`go` failure) or 2
(bad flags).

A Deployable is affected when, between base and head:

- a changed or deleted file belongs to a Go package in the main module that is
  in the dependency closure of `cmd/<name>` (built for `linux/amd64`,
  `CGO_ENABLED=0`, whatever the workflow's `platforms`; any change to a
  source file in a package's directory counts, whatever its build
  constraints, so only a package imported solely on another platform is
  missed). A file belongs to a package when it's a source file
  (`.go`, `.s`, `.c`, `.syso`, …) in the package's directory or a file the
  package embeds;
- its `.forge.yaml` entry changed in any key (`kind`, `port`, `schedule`,
  `base_image`, `build`, …), or it's new;
- it uses `build: dockerfile` and `cmd/<name>/Dockerfile` changed;
- `go.mod` or `go.sum` changed (every Deployable is affected).

It needs `git` and `go` on `PATH`. Resolving the dependency closure runs
`go list`, which may download modules.
