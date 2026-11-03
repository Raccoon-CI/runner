# raccoonci-runner

[中文](README.zh.md)

The machine side and own engine of Raccoon CI: it runs GitHub Actions workflows on the machine it is started on (`run`), waits for a controller's job (`agent`), runs one job instance (`run-instance`) and builds the InstaBox runner snapshot (`runner-snapshot`). The public `pkg/*` packages (`actions`, `cliflag`, `cliutil`, `engine`, `runner`, `runnercmd`, `scan`) are imported by the cli repository; they are unstable (0.x).

## Build and test

```sh
make build           # dist/raccoonci-runner
make runner-binary   # static linux/amd64 binary for the runner snapshot
make check           # vet + race tests
dist/raccoonci-runner run .github/workflows/ci.yml
```

## Other repositories

`lib` (shared library, required) <- this repository <- `cli` (the `raccoonci` command); `backend` (server) depends on lib only. It requires lib `v0.4.0`; this repository's latest tag is `v0.2.0`. See CLAUDE.md and TODO.md.

## Licence

Apache-2.0, see LICENSE; third-party notices in THIRD_PARTY_NOTICES.
