# GitHub Actions internal-API compatibility survey

Investigated 2026-10-28 by reading the source of `actions/toolkit` (the library official actions use), `nektos/act` (MIT, Go) and `go-gitea/gitea` (MIT, `routers/api/actions/`).

## Which runtime services official actions depend on

The runner injects environment variables at job start. The toolkit packages use them to reach GitHub's internal services:

| toolkit package | Env vars used | Protocol | Typical actions |
| --- | --- | --- | --- |
| `artifact` | `ACTIONS_RESULTS_URL`, `ACTIONS_RUNTIME_TOKEN` | Twirp `github.actions.results.api.v1.ArtifactService` (v4); v3 is the legacy REST `/_apis/pipelines/workflows/:runId/artifacts` | `upload-artifact`, `download-artifact` |
| `cache` | `ACTIONS_CACHE_URL`, `ACTIONS_RESULTS_URL`, `ACTIONS_RUNTIME_TOKEN`, `ACTIONS_CACHE_SERVICE_V2` | v1 REST `/_apis/artifactcache/...`; v2 Twirp `CacheService` | `actions/cache`, the `cache:` option of the `setup-*` actions |
| `core` | `ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN` | OIDC token endpoint | `aws-actions/configure-aws-credentials`, `google-github-actions/auth` |
| `core` | `GITHUB_STEP_SUMMARY` | Local file, uploaded by the runner | Job summaries |
| `github` | `GITHUB_API_URL`, `GITHUB_TOKEN` | Ordinary GitHub REST/GraphQL | `github-script`, release and PR-comment actions |
| `attest` | `ACTIONS_ORCHESTRATION_ID` + OIDC | Sigstore | `attest-build-provenance` |

Key detail (`packages/cache/src/internal/config.ts`): when `GITHUB_SERVER_URL` is not github.com/ghe.com, cache **is forced to v1**. That means a self-hosted implementation of the v1 REST protocol is enough, and v2 never needs to be supported.

## Who has implemented what

| Capability | act | Gitea | Raccoon CI plan |
| --- | --- | --- | --- |
| Artifact v3 | ✅ `pkg/artifacts/server.go` | ✅ `artifacts.go` | Implement |
| Artifact v4 (Twirp) | ✅ `pkg/artifacts/artifacts_v4.go` | ✅ `artifactsv4.go` | Implement (current `upload-artifact@v4+` uses it) |
| Cache v1 | ✅ `pkg/artifactcache` | Provided by act_runner's embedded act cache server | Implement |
| OIDC id-token | ❌ | ✅ `oidc.go` (issuer + JWKS + token) | Implement. Users must add our issuer to the cloud's trust config |
| Job summary | ❌ | ✅ `job_summary.go` | Implement |
| `GITHUB_TOKEN` / API | Passes a user-supplied token | Its own GitHub-compatible API (partial) | Repos stay on GitHub, so issue a **GitHub App installation token** and call the real GitHub API. Best compatibility |

## Things that are hard or impossible to replicate

- **`ubuntu-latest`'s preinstalled software** (the actions/runner-images image). This is the biggest real-world compatibility gap. act works around it with large community images. Raccoon provisions on demand instead (see "On-demand environments" below).
- **`attest-build-provenance`**: the public Sigstore instance only trusts GitHub's OIDC issuer.
- **`deploy-pages`**: the GitHub Pages API verifies GitHub's own OIDC tokens.
- **Cloud OIDC trust** (AWS/GCP): this works, but users must change their cloud-side trust config from `token.actions.githubusercontent.com` to the Raccoon issuer. The YAML doesn't change.

## Reuse notes

act and Gitea are MIT-licensed, so their code can be referenced or reused with attribution. act's `pkg/model` (workflow parsing) and `pkg/exprparser` (`${{ }}` expressions) can be considered as dependencies directly. Forgejo is under GPLv3 in recent versions, so do not copy its code.

## On-demand environments (toolcache prefill)

`@actions/tool-cache` `find()` (`packages/tool-cache/src/tool-cache.ts`) checks for `${RUNNER_TOOL_CACHE}/<tool>/<version>/<arch>` and a sibling `<arch>.complete` marker. If both exist, the `setup-*` action uses that copy and skips the download. So Raccoon can pre-install exactly the versions a workflow asks for, in the same layout, without modifying any action.

Where demands come from (they must be resolved per job, after matrix expansion):
- `with:` inputs of `setup-*` actions: `node-version`, `go-version`, `python-version`, `java-version`, ...
- Version files those inputs point to: `node-version-file: .nvmrc`, `go-version-file: go.mod`, `.python-version`, `.tool-versions`. These need the repo at the triggering commit.
- Implicit demands are invisible to a static scan: `run: make` assumes gcc, `jq`, `docker` etc. exist on `ubuntu-latest`. Cover the common ones with a slim base image, detect others heuristically from `run:` scripts, and learn from `command not found` failures.

Decision (2026-10-28): **do not bake toolchains into snapshots.** Snapshots for every demand combination get large and multiply, and downloading from the public internet at job start is fast enough to feel instant. Snapshots/base images hold only the slim base. Before the job starts, Raccoon downloads each resolved demand into the toolcache layout, in parallel. Version-file demands are fetched right after checkout.

The scanner is **in the MVP** (decided 2026-10-28). Its main job is catching careless workflows: on `ubuntu-latest` they quietly rely on preinstalled tools and never declare a dependency. They would break on our slim base. Detection sources, strongest first:
1. Explicit `setup-*` inputs and the version files they reference.
2. Commands used in `run:` scripts (`npm`, `python`, `go`, `docker`, `jq`, ...). Extract them with a real shell parser (e.g. `mvdan.cc/sh`), not regex.
3. Repo manifests at the triggering commit: `package.json`, `go.mod`, `pyproject.toml`/`requirements.txt`, `Cargo.toml`.
4. At runtime, exit code 127 / `command not found`: record it so the next run installs the tool.

When a tool is detected without a declared version, install the same default version that `ubuntu-latest` ships, so behavior matches GitHub.
