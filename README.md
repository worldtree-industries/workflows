# workflows

Shared reusable GitHub Actions workflows for worldtree-industries Go projects.

Vocabulary (Project, Deployable, Tool, Kind, Manifest) follows
[ivaldi's GLOSSARY.md](https://github.com/worldtree-industries/ivaldi/blob/main/GLOSSARY.md).

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
  `CGO_ENABLED=0`). A file belongs to a package when it's a source file
  (`.go`, `.s`, `.c`, `.syso`, …) in the package's directory or a file the
  package embeds;
- its `.forge.yaml` entry changed in any key (`kind`, `port`, `schedule`,
  `base_image`, `build`, …), or it's new;
- it uses `build: dockerfile` and `cmd/<name>/Dockerfile` changed;
- `go.mod` or `go.sum` changed (every Deployable is affected).

It needs `git` and `go` on `PATH`. Resolving the dependency closure runs
`go list`, which may download modules.
