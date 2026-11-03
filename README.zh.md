# raccoonci-runner

[English](README.md)

Raccoon CI 的机器侧与自有引擎：在当前机器上运行 GitHub Actions workflow（`run`），等待控制端派来的 job（`agent`），运行单个 job 实例（`run-instance`），并构建 InstaBox runner 快照（`runner-snapshot`）。公开的 `pkg/*`（`actions`、`cliflag`、`cliutil`、`engine`、`runner`、`runnercmd`、`scan`）由 cli 仓库引用，对外不稳定（0.x）。

## 构建与测试

```sh
make build           # dist/raccoonci-runner
make runner-binary   # 给 runner 快照用的静态 linux/amd64 二进制
make check           # vet + race 测试
dist/raccoonci-runner run .github/workflows/ci.yml
```

## 与其他仓库的关系

`lib`（共用库，必需）<- 本仓库 <- `cli`（`raccoonci` 命令）；`backend`（服务端）只依赖 lib。本仓库依赖 lib `v0.4.0`，最新标签是 `v0.2.0`。详见 CLAUDE.md 与 TODO.md。

## 许可证

Apache-2.0，见 LICENSE；第三方声明见 THIRD_PARTY_NOTICES。
