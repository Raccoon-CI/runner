# CLAUDE.md

Guidance for Claude Code (claude.ai/code) in this repository.

## Project

`github.com/Raccoon-CI/runner` is the machine side and the own engine of Raccoon CI, a self-hosted CI/CD platform that runs GitHub Actions workflow YAML on compute the user chooses. It was split out of the `raccoonci` monorepo (2026-11-03, history kept). Runners are Linux only; InstaBox machines run Debian 13 (trixie), not Ubuntu, even for jobs that say `ubuntu-latest`.

The own engine executes a workflow itself: it parses, expands matrices, scans what each job instance needs installed, installs it, and runs the steps the way GitHub's runner does, including the runtime services official actions call (see `docs/gha-compat.md`).

## Depends on

- `github.com/Raccoon-CI/lib` (value, expr, workflow, trigger, catalog, runnerimages, platform, wire, dispatch, runevent, jobgraph, instabox, blob, artifact, cache, archive, ...).
- Nothing else of ours. Dependency direction: lib <- runner <- cli, and lib <- backend. The backend never imports this module; the cli imports its public `pkg/*`.

## Modules and layout

- `cmd/raccoonci-runner`: the only binary: `run [flags] <workflow.yml>` (this machine only, no `-on`), `agent`, `run-instance`, `runner-snapshot`, `-version`. Built-in catalog only. `version` is set with `-ldflags "-X main.version=..."`.
- **Public packages, imported by the cli repository. Unstable outward: 0.x, exported names may change in any release.**
  - `pkg/actions`: parses `action.yml` (unknown keys ignored, as GitHub does) and downloads actions.
  - `pkg/cliflag`, `pkg/cliutil`: flag and helper code shared by the commands of both binaries.
  - `pkg/engine`: composes `jobgraph` (lib) with the local executor that scans, provisions and runs each instance (`runHere`), or scans and calls `Options.Remote` (`runRemote`). `Run` and `RunInstance`.
  - `pkg/runner`: runs one job instance the way GitHub's runner does (`run:` steps with GitHub's exact shell command lines, JS/composite/Docker actions, container jobs and `services:` through the docker CLI, workflow commands and file commands, masking, timeouts, post steps).
  - `pkg/runnercmd`: the machine-side commands (`Run`, `Agent`, `RunInstance`, `Snapshot`) that `cmd/raccoonci-runner` and the cli's `raccoonci` both call.
  - `pkg/scan`: a static, best-effort scan of what a job instance needs installed (explicit `setup-*` demands, implicit commands from `run:` scripts parsed with `mvdan.cc/sh`, composite action steps). It never fails a job; what commands and actions mean comes from the catalog.
- `internal/provision` (installs scanned demands into the tool cache in toolkit's layout; never fails the job), `internal/nodejs`, `internal/semver`, `internal/mirror` (`RACCOONCI_MIRROR_<NAME>` base URLs), `internal/missing` (commands a job could not find, remembered per workflow and job), `internal/docker`, `internal/local` (github context from git, checkout by copying the tree, terminal output).
- `docs/gha-compat.md`: the survey of the runtime services actions depend on.

Knowledge that changes as tools evolve belongs in the catalog (`pkg/catalog/catalog.yaml` in lib), not in Go code here. Python comes from python-build-standalone, not GitHub's builds (setup-python refuses them on Debian).

## Commands

```sh
make build                # dist/raccoonci-runner (VERSION=x.y.z sets main.version)
make runner-binary        # static linux/amd64 dist/raccoonci-runner-linux-amd64 for the InstaBox snapshot
make test | race | vet
make check                # vet + race tests — must pass before merging
go test ./pkg/runnercmd -run TestName -v
```

Use `GOFLAGS=-p=1` (memory). The `pkg/runnercmd` tests build `cmd/raccoonci-runner` themselves.

## Developing before the tags exist

`../go.work` (the directory above the four repositories) uses `lib`, `runner`, `cli`, `backend` and replaces `lib v0.1.0` and `raccoonci-runner v0.1.0` with the local directories, so build and test work offline. go.mod requires `lib v0.1.0`, which is not tagged yet: do not run `go mod tidy` until the tags exist (then run it once; unused requirements left over from the split are harmless meanwhile). CI fails until lib v0.1.0 is tagged.

## Known issues

- **git bisect:** in the original monorepo history a stretch of commits has an `engine.go` that does not compile (the job summary fields were committed in the wrong places). Here the stretch starts at `dcee076` "Server: a job's check run carries its summary and the annotations its steps printed", ends at `e4e472f` "Split step S2: the events and results a runner reports live in runevent, and wire rests on data only", and is fixed by `a9aaa18` "Fix engine.go: the job summary fields were committed in the wrong places" (14 commits from `dcee076` to `a9aaa18`). `git bisect skip` them (taken from the original history; not re-verified by building each commit here).
- The runner snapshot (`runner-snapshot`, lib's `instabox.BuildRunnerSnapshot`) still installs the binary as `raccoonci`; it has to install `raccoonci-runner` (TODO.md).
- Not supported yet: reusable workflows, problem matchers (see TODO.md B.4/B.5).
