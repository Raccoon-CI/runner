# Raccoon CI runner 仓库的待办（B：自有引擎）

本仓库是从 raccoonci 单仓库拆出来的 runner（2026-11-03 拆分），只保留原 TODO 的 **B 部分**：自有引擎（`raccoonci-runner run`、`agent`、`run-instance`）。A（GitHub runner 路径）在 cli 仓库，C.1（服务端）在 backend 仓库，C.2（算力提供方）和 catalog feed 在 lib 仓库。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

## 拆分后的遗留（本仓库）

- ⬜ **runner 快照仍然安装 `raccoonci`，需要改装 `raccoonci-runner`。** `runner-snapshot`（`pkg/runnercmd/runnersnap.go`）和 lib 里的 `instabox.Client.BuildRunnerSnapshot` 把一个静态二进制装成机器上的 `raccoonci` 并起 agent 服务；拆分后机器侧的二进制是 `raccoonci-runner`（`make runner-binary` 现在产出 `dist/raccoonci-runner-linux-amd64`）。要改：安装路径与服务的 `ExecStart`、帮助文字、`-binary` 说明，以及 cli 里 `run -on instabox` 对快照里二进制名的假设；改完要重建快照并实测。**拆分时没动，行为不变。**
- ⬜ 各 `pkg/*` 的导出名对外不稳定（0.x），lib/runner 已打 v0.1.0 标签（`go mod tidy` 已跑过），收紧要等真实的外部使用者出现。
- ⬜ 原 `runner-images update` 命令（刷新内嵌的 GitHub 镜像快照）在 cli 仓库，本仓库的 Makefile 不再有 `runner-images` 目标。

# B. 作为独立 CI/CD 平台（`raccoonci run`，自有引擎）

Raccoon CI 自己执行 workflow，所以要自己实现 GitHub runner 和 GitHub 服务端本来提供的东西：解析、求值、装环境、执行、artifact/cache/OIDC 等运行时服务、job 类型、workflow 语义。长期运行的服务和算力见 C。

## B.1 已完成

**解析与求值**
- ✅ workflow 严格解析：拒绝未知字段、重复 key、环依赖，带行号；防 YAML alias 炸弹（`pkg/workflow`、`pkg/value`）
- ✅ matrix：include/exclude 规则按 GitHub 文档示例实现；256 job 上限；运行时展开（`fromJSON(needs…)` 动态 matrix）
- ✅ 表达式 `${{ }}`：词法/语法/求值，GitHub 的类型转换、大小写不敏感比较、`&&`/`||` 返回操作数、`*` 过滤、全部内置函数（`pkg/expr`）
- ✅ `if:` 条件：隐式 `success()`、`failure()` 对祖先 job 传递、job 级 skipped 传播

**环境知识**
- ✅ `runnerimages`：GitHub 官方镜像预装了什么（默认版本、缓存版本、apt 包、CLI）。优先读 README 状态 badge（不占 API 额度），失败回退到 releases API；内置 3 个镜像的快照作离线兜底
- ✅ `catalog`：把"命令属于哪个工具链、setup action、版本文件、runner 标签、镜像、Ubuntu→Debian 警告"等全部做成数据（`catalog.yaml`），内置默认值
- ✅ catalog 拉取：`-catalog-url` 拉 feed，ETag + 每日刷新，离线用缓存，坏内容不覆盖好缓存，可选 ed25519 签名，只允许 https，名字做字符集校验防注入
- ✅ 扫描器 `scan`：`setup-*` 版本（输入/matrix/版本文件）、`run:` 脚本里没声明的命令（用 shell 解析器）、不透明构建命令（`make` 等）结合仓库清单文件推断、Ubuntu 专属写法警告、YAML 浮点版本（`3.10`→`3.1`）警告
- ✅ `plan` 命令：列出 job、matrix 组合、每个组合要装什么，并提前警告不可用的 OS

**执行**
- ✅ `runner`：`run:` 步骤（GitHub 同款 shell 命令行）、`continue-on-error`、步骤/job 超时、取消（SIGINT→SIGKILL，`always()` 与 post 步骤有 5 分钟宽限）
- ✅ workflow 命令（新旧两种格式）和文件命令（`$GITHUB_ENV/OUTPUT/PATH/STATE/STEP_SUMMARY`）；密钥在日志中打码
- ✅ JS action（pre/main/post、`INPUT_*`、`STATE_*`）、composite action；Node 运行时从 nodejs.org 下载并校验 SHA256
- ✅ `engine`：job 依赖图、job 级 `if`、fail-fast、`max-parallel`、job 级 `continue-on-error`；不可用 OS 的实例明确失败，其余照常（不静默跳过、不回退 Linux）
- ✅ 安全：下载的压缩包无法写出目标目录；仓库读取限制在仓库内并限大小
- ✅ CLI：`plan`、`run`、`catalog update`、`runner-images update`

**装环境**（`internal/provision`、`internal/semver`）
- ✅ 每个 job 启动前自动扫描并安装。作业用了 `node` 却没声明，就装镜像默认版本并放到 PATH 最前；声明了 `setup-node` 的，把 toolcache 预先填好，官方 action 直接命中缓存、不再下载
- ✅ 安装器：Node.js（nodejs.org，支持 `lts/*`、`lts/iron`、`lts/-1`、`latest`）、Go（go.dev，支持 `stable`/`oldstable`）和 Python（python-build-standalone），下载校验 SHA-256，解压到私有目录再原子改名；并发的 job 共用一次下载；中断留下的半成品会被替换
- ✅ **Python 在 Debian 上能用了**：实测 `setup-python` 在干净的 Debian 13 上直接失败（GitHub 的 Python 构建是给 Ubuntu 的）；把 Python 预先放进 toolcache 后它直接命中，`Successfully set up CPython (3.12.16)`。隐式用到的 `python3`（镜像默认 3.12.3，已不再发布）放宽到同一小版本的最新补丁；workflow 里显式写死的版本绝不偷换，而是给出明确提示
- ✅ 版本规格：`20`、`20.x`、`>=18`、`^1.22`、`~1.2.3`、`1.2 - 1.4`、`||`；缓存里已有满足的版本就不联网（离线可用）
- ✅ apt：`-install=all` 时用 sudo 安装缺少的包，Ubuntu→Debian 包名用 catalog 的 `debian:` 字段；一个包装不上不影响其余；默认不装，只报告
- ✅ 装环境失败只给警告，不会让作业失败（扫描结果只是启发式）

**真实环境实测过的**：`actions/setup-node@v4` 真实运行；`runner-images update` 联网成功；`setup-node` 输出 `Found in cache`，说明预填被官方 action 认可。在一台**全新的 InstaBox Debian 13 沙箱**（没有 go、make、gcc、tree、pip）上，一个没声明任何版本的 job 用 5 秒装好 node 22.23.3、go 1.24.13、make、gcc、tree 并通过；`setup-python@v5` 修复后通过。其余网络相关代码只有假服务器测试。

**测试机**：为此建过一台 InstaBox 沙箱 `test-machine`（`m_EXAMPLE_MACHINE_ID`）。2026-10-28 核对时它已不在当前账号的机器列表里，无需再处理。

**文档**：`CLAUDE.md`（架构与约定）、`docs/gha-compat.md`（GHA 运行时依赖调研，参考 act / Gitea）

## B.2 装环境——剩余部分

Node.js、Go、Python、Java、Rust、apt 包已经通了（见 B.1 和下面的条目）。2026-11 收尾：运行时兜底和下载镜像已做，其余没做的条目移到本节末尾的"搁置"，不再算 B.2 的未完成项。

- ✅ 运行时兜底（`internal/missing`）：步骤以 127 退出且输出里有 `<cmd>: command not found` 时，runner 发一条警告并通过 `Config.OnMissing` 上报命令名；引擎把它记在 `<user cache>/raccoonci/missing/<repo>.json`（按 workflow 文件和 job id，最多 32 个，名字只接受字母数字和 `._+-`）。下次这个 job 扫描时，`scan.Input.Missing` 里 catalog 认识、镜像又预装的命令当作隐式需求，走和 `run:` 里的命令同一条路径。只在本机运行（`-install` 不是 none）时记；`-on instabox` 的机器用完即删，不记。单测：runner（127 有报告、`exit 1` 里打印 not found 不报告）、store、scan。**没做**：catalog 不认识的命令只警告，不会自动装；没在真实 job 上跑过两次验证第二次补装成功，只有单测。
- ✅ 下载镜像配置（`internal/mirror`）：环境变量 `RACCOONCI_MIRROR_<NAME>` 替换下载基地址，NAME 为 `NODE`、`NODE_MUSL`、`GO`、`PYTHON_LATEST`、`PYTHON_RELEASE`、`RUST`、`JAVA`、`ACTIONS_API`、`ACTIONS_CODELOAD`。只读环境变量，不进 catalog feed（feed 不能重定向下载）；值必须是不带用户名密码的 http(s) URL，不合法就忽略并警告。校验和仍然检查，但校验文件也从镜像取，所以镜像和源站同等被信任。设了 `ACTIONS_API` 时 `actions.NewStore` 丢掉 GitHub token，不发给镜像。**没做**：runner-images 快照的下载（`runnerimages`）、InstaBox 和 GitHub API 的地址不走这里；没用真实镜像站测过，只有单测。
- ✅ Java（Temurin，Adoptium 接口）和 Rust（static.rust-lang.org 的 rustc/cargo/rust-std/rustfmt/clippy 组件包，逐个校验 SHA-256 后用各自的 install.sh 装）的安装器（2026-10-29）。两者在 catalog 里标了 `implicit-only`：`setup-java`、rustup 自己会下载，预填 tool cache 它们不会读，所以只给“没写 setup 就直接用 `cargo`/`java`/`mvn`”的 job 装。真实网络实测：一个 job 隐式用 cargo、rustc、cargo fmt、java，11 s 装好。Rust 只支持 `stable` 和精确版本（1.80、1.80.0），Java 只支持主版本和精确版本（17、17.0.9、1.8），范围写法（`>=1.70`、`lts`）不支持；没有探测机器自带的 Rust 和 Java，总是下载
- ✅ 采用机器自带的工具链：InstaBox 的 `universal` 模板自带 Go、Node 等。装环境前先按 catalog 的 `probe`（运行什么命令、从输出哪一段取版本号）探测，版本满足就把安装目录链接进 toolcache，不再下载。运行时探测，不写死模板里有什么。实测：全新缓存目录下跑自己的 CI，`go 1.27.1 (already on this machine)`，`setup-go` 直接 Found in cache。目前只给 node 和 go 配了 probe；Python 不采用系统版本（`setup-python` 要的布局不同），其余工具链（Rust、Java 等）还没有安装器也没有 probe
- 🟡 项目环境快照（装完依赖拍快照，锁文件不变就复用）：键和登记表已做（`internal/envsnap`）。键 = 锁文件内容哈希 + 用到的工具链名 + 机器镜像标识，不含工具链版本；`plan` 会显示每个实例能否快照。真实项目上：cobra/本项目（go.sum）、commander.js（package-lock.json）、click（uv.lock）可以；express 没有锁文件，判定为不能快照。**已接到 `-on instabox`（`-env-snapshots N`，默认关）**：实例启动前按键查登记表，命中就从该快照建机器，快照已被删（400/404）则忘掉并退回 runner 快照重跑；未命中时，成功的一次运行会在后台清理机器（删 agent 配置和本次 workspace，保留各种缓存）、拍快照、再删机器，同一个键同时只有一台机器拍；超出上限的旧快照由 API 删除。只在可信事件（非 `pull_request*`）、且没有 `-secret` 时才拍。**monorepo**：键只含仓库根的锁文件，所以用到的工具链在子目录里有清单或锁文件（跳过 `.git`、`node_modules`、`vendor` 等）时，判定为不能快照（`ErrUnpinned`），避免子目录依赖变了却复用旧快照；把子目录锁文件也算进键还没做。**还没做**：**真实 InstaBox 端到端已验证（2026-10-29）**：带 uuid 依赖的小 Go 项目，三次运行：①未命中，建机器 10.8 s，构建 4.2 s（下载依赖），成功后保存快照；②命中，机器 7.7 s，构建 0.3 s，整条命令 35 s → 10.6 s；③改 go.sum 后键变了，未命中并保存新快照，旧快照被淘汰（上限 1）。测试机器自动删除，测试快照和端口已手工清理。**另一个实测发现（本项目 ci.yml，`-on instabox`）**：`actions/setup-go` 的缓存（747 MB，含 go-build）从主控恢复用了 2m26s，比什么都不缓存的冷跑（总共 1m45s）还慢——主控（这台 InstaBox 机器）出口约 6 MB/s。所以在"主控本身在 InstaBox 上"的开发配置里，开着环境快照时 `setup-go` 的缓存恢复反而拖慢整个 job；正式的主控要放在带宽足够的地方，或把缓存对象放对象存储，机器直接从存储下载。**缓存对象放 S3 已做（2026-10-29）**：`run -cache-s3-bucket B [-cache-s3-endpoint URL -cache-s3-path-style -cache-s3-region R -cache-s3-prefix P]`，密钥只从环境变量 `RACCOONCI_CACHE_S3_KEY`/`_SECRET`（可选 `_TOKEN`）读；每个仓库一个子目录；下载是预签名链接，机器直接找存储；上传仍经过主控（缓存协议是对 Azure Blob 的模拟，直传要另改）。用本地 SeaweedFS 的 S3 网关验证了功能：1 GB 的 setup-go 缓存保存成功、第二次运行能找到并下载。**没有在真实外部桶上量过速度**——本地 SeaweedFS 和主控在同一台机器上，量不出带宽差别。在 InstaBox 机器上实测：入站从公共 CDN 下载 78 MB 用 0.42 s（约 185 MB/s），出站约 6 MB/s 且是每台机器各自的限制（两台同时传互不影响，同机两个并发上传共享这 6 MB/s），所以缓存下载应该会快很多，保存缓存（上传）不会。验证中发现并修了一个真问题：S3 的分片是流（`LimitReader`），SDK 重试时倒不了带（"request stream is not seekable"），存储偶尔 5xx 就让保存直接失败；现在不超过 32 MiB 的分片先读进内存再发（`maxBufferedPart`）
- ✅ 通用 runner 快照（InstaBox）：`universal` 模板（Debian 13）+ 静态编译的 `raccoonci`（`/usr/local/bin`）+ 清单 `/etc/raccoonci-runner.json`，不含工具链、依赖、机密、登录信息。做法：新建一台干净的 `universal`，临时用 `instabox-port serve` 发布只含二进制的目录（随机路径），机器上下载并校验 SHA-256 后立刻撤下端口，检查母机无凭据文件后拍快照。实测：从快照建机器约 9.5 s，在上面用快照里的 `raccoonci` 跑 checkout + `setup-go` + `setup-node` + 运行步骤，7.2 s 成功，Go 和 Node 都直接采用机器自带的版本。这次验证还发现并修了一个真问题：模板把 Go 和 Node 装在同一个 `/usr/local/bin`，采用时不能把整个前缀链接进某个工具链的目录，否则会把别的工具带上 PATH，盖掉前面步骤选的版本（已修，见 `sharesPrefix`）。**更新方式还是手工的**（每次新版本要重做快照）；要有自动化，并在快照清单里校验 `raccoonci` 版本和模板版本（后来做了 `raccoonci runner-snapshot`，见 C.2）
- ✅ 第三方 action 内部的依赖（2026-10-29）：扫描会下载 catalog 里没有专门规则的 action 的 `action.yml`（`scan.Input.Action`，引擎里用 `actions.Store.Lookup`，和 runner 共用同一个 Store，所以不多下载），docker action 记 docker；composite action 递归扫它的步骤（最深 4 层，自己调自己会停），内部的 `setup-*`、`run:` 里的命令都按普通步骤算；`inputs` 用调用方的 `with`，没有就用 action 的默认值，运行时才知道的输入不会漏出占位符当版本；内部步骤的 `if:` 能判断为假就跳过（`runner.os` 当作 Linux，所以只给 Windows/macOS 的步骤不会被当成需求），判断不了就保留。仓库里的本地 composite action（`./path`）也读 `action.yml` 扫。`raccoonci plan -fetch-actions` 可以在不运行时看到这些（默认不联网）。在 80 个真实 workflow 上对比：13 个多了新需求，例子：`ossf/scorecard-action` 是 docker action；`prometheus/promci/build` → `promci-setup` 两层 composite 里要 go、make、docker；`golang/govulncheck-action` 内部的 `setup-go stable`；`curl`、`gh`、`pwsh`。**还没做**：外层 workflow 自己步骤的 `if:`（比如 `runner.os`、matrix 条件）也还没用来跳过不会运行的步骤；`-on instabox` 时主控这边没有 Store，所以环境快照的键不含 action 内部的工具链（机器那边的扫描是完整的）；JS action 的 `package.json` 依赖不看（它们自带 node_modules）；docker action 的 `Dockerfile` 内容不看
- 🟡 工具链缓存清理（2026-10-29）：`raccoonci run -toolcache-limit-gb N`（默认 10，0 为不限），本机运行开始时按“最久没用”淘汰版本，直到不超上限；24 小时内用过的不删，所以上限是软的。“最近使用”记在 `.complete` 标记的修改时间上，每次选中某个版本都刷新；超过 24 小时的中断安装残留（`.part-*` 等）顺手清掉；采用的机器自带版本只是一串链接，不计大小。工具缓存在用户缓存目录（InstaBox 上是 `/data/home/.cache`，持久盘），不在 `/tmp`。**没做**：远程模式下机器上的缓存不清理（每台机器用完即删）；`node/`（Node.js 运行时，实测 750 MB）和 `runs/`（工作目录，765 MB，只增不减）不在这个上限里；只在运行开始时清一次，一次运行里装的东西不会在运行中被淘汰；setup-* 动作自己写进工具缓存的版本也会被淘汰，下次由它们重新下载

**搁置（不再算 B.2 的未完成项）**：下面几条只在自有引擎跑在非 Ubuntu 机器（Debian、用户自己的机器）时才有意义。InstaBox 的 `ci` 模板是 Ubuntu 24.04 加 GitHub 的 runner 镜像，`serve` 路径用它；用户认为自有引擎也能跑在 `ci` 模板上（**未验证**：当前 runner 快照由 `universal`（Debian 13）做成，`raccoonci runner-snapshot` 写死了这一点，且 catalog 里的 Debian 13 平台、`lsb_release`/PPA 警告和 `probe` 采用自带 Go/Node 的做法在 Ubuntu 上都要重新看；这件事本身放在 C.2）。**`ci` 模板的预装内容已实测（2026-11，起一台机器查完即删）**：Ubuntu 24.04.5，x86_64，glibc 2.39，`RUNNER_TOOL_CACHE=/opt/hostedtoolcache`。tool cache 里有 Go 1.24.13/1.25.14/1.26.10、Node 22.23.4/24.22.1、Python 3.11.18/3.12.16/3.13.18/3.14.10/3.15.2、PyPy、Ruby 3.2.11/3.3.13/3.4.12/4.0.9、Temurin 8/11/17/21/25；PATH 上有 ruby 3.2.3、gem、node 22、go 1.24、java 17、mvn、gradle、cargo/rustc、gh、kubectl、helm、yarn、docker、jq、git、gcc、make。**没有**：terraform、`bundle`（`gem install bundler` 才有）、`python3.10`（tool cache 里最老的 Python 是 3.11，`setup-python` 在 Ubuntu 上会按 GitHub 的清单下载 3.10）、`ruby-build`、`raccoonci`。所以 Ruby 和 CLI 工具在这个模板上基本不用装，但 terraform 和旧版 Python 要靠 `setup-*` 或下载。

- ⬜ Ruby 的安装器。`ruby/setup-ruby` 只认 `ubuntu-22.04/24.04/26.04`、macOS、Windows（读过它的 `common.js`）；Debian 被当成自托管，只去 `$RUNNER_TOOL_CACHE/Ruby/<版本>/<arch>` 找，没有就报错让用户自己 `ruby-build`。要在 Debian 上支持，得预填 tool cache：`ruby-build` 编译（每个版本几分钟，要 gcc、libssl-dev、libyaml-dev）或试 ruby-builder 的 Ubuntu 预编译包（没试过，包里可能写死了 `/opt/hostedtoolcache`）
- ⬜ 旧版 Python（3.10 及更早、或旧补丁）：最新一期 python-build-standalone 不含，要翻更早的发布。**不必走 GitHub API**：下载地址 `releases/download/<tag>/…` 本来不走 API，tag 列表可用 `git ls-remote --tags` 或 Atom feed；带 token 时 API 限额是 5000 次/小时（沙箱里的 gh 已登录，实测剩余 4931），只有无 token 的用户机器才是 60 次/小时
- ⬜ 各种 CLI 工具的安装器：docker、gh、kubectl、helm、terraform、yarn、maven 等。目前只检查是否已在 PATH 上，缺了就警告
- ⬜ 其他架构：toolcache 的 arch 目录已按机器架构命名，但只在 x64 上实测过；没有 arm64 机器可测

## B.3 运行时服务（GHA action 依赖的内部接口）

- ✅ artifact v4（Twirp + Azure Blob 的 Put Block / Put Block List）：`pkg/artifact`、`pkg/blob`（本地目录和 S3 兼容存储）。`raccoonci run` 里 `upload-artifact@v4` 与 `download-artifact@v4` 跨 job 实测通过（20 MB，sha256 一致）
- ✅ 按 job 签发 token：`-on instabox` 下每个实例一个 token（`services.jobEnv`，scope 里的 job id 是 `<job>-<n>`），只能以自己的 job 创建 artifact，别的 job 的 artifact 仍可读（和 GitHub 一样）。`remote_test.go` 有单测。**没做**：本机 `raccoonci run` 仍是整个 run 共用一个 token
- ⬜ artifact 剩余（搁置，见本节末尾；元数据持久化和 retention 清理服务端已做，见 C.1 的 artifact 一条）：整体 sha256 校验（现在只记录）；在真实 AWS 上验证 S3（目前只测过 SeaweedFS）；`download-artifact` 的按 ID 跨 run 下载（需要 GitHub token）
- 🚫 artifact v3（REST）：不做。GitHub 已弃用 v3，用 v3 的 workflow 应该升级到 v4
- ✅ cache v1（`/_apis/artifactcache`）：`pkg/cache`。真实 `actions/cache@v4` 与 `cache/restore@v4` 实测（20 MB、1 MiB 分块、两个 job；第二个进程命中第一个进程存的缓存）。缓存按仓库放在用户缓存目录，跨次运行保留
- ✅ cache 淘汰与大小上限：`-cache-limit-gb`（默认 10，和 GitHub 一样，0 不限）超了就淘汰最久没用的，单个条目超过上限直接拒绝；提交前按 `statfs` 检查磁盘剩余，不够就先淘汰、仍不够返回 507。`pkg/cache/limit_test.go` 有 v1/v2 的单测
- ✅ cache 放 S3 兼容存储：`-cache-s3-bucket`（凭据走 `RACCOONCI_CACHE_S3_KEY/_SECRET/_TOKEN`），条目放桶里，客户端从桶的预签名链接下载。对着 SeaweedFS 的 S3 网关验证过，没有试过真实的外部桶
- ⬜ cache 剩余（搁置，见本节末尾）：按分支隔离（现在同仓库所有 ref 共用）；多个 `raccoonci` 进程同时写同一个 index（后写覆盖）；下载链接与 run 共用同一把密钥
- ✅ cache v2（Twirp `CacheService`：CreateCacheEntry / FinalizeCacheEntryUpload / GetCacheEntryDownloadURL）：`pkg/cache/v2.go`，上传那一半抽成了 `internal/azblob`（新增单次 Put Blob）。`raccoonci run -cache-protocol v2` 下用真实 `actions/cache@v4` 实测：20 MB（单次 Put Blob，第二个进程命中）、150 MB（超过 SDK 单次上限 128 MiB，走分块），各跑两个 job。v1 与 v2 共用同一批缓存条目
- ⬜ cache v2 剩余：默认协议仍是 v1（toolkit 只有设了 `ACTIONS_CACHE_SERVICE_V2` 才走 v2，服务端不会按 toolkit 版本自动选）；请求里的 `CacheMetadata`（repository_id、scope）被忽略；150 MB 那次没有确认分块走的是直通路径还是落盘路径
- 🟡 OIDC token 端点（2026-11-02，**未提交**，服务端 `internal/server/oidc`）：**设计取舍，先按最稳妥的做，等你拍板**——①签发者是服务端自己（`<public-url>/oidc`，所以每个云账号要为这个签发者设一次信任；而不是接外部身份）；②RS256，密钥用主密钥封存存库（没有主密钥则每次启动一把新的，验证方重新取公钥集即可）；③request token 是进程内随机密钥的 HMAC，绑定 run+job、run 结束后失效，重启后随 run 重排队而重发；④只有 job（或 workflow）有 `id-token: write` 的才给 `ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN`，fork 的 PR 永远不给；⑤claim 用 GitHub 的名字和 `sub` 形式（`repo:o/r:ref:<ref>` / `repo:o/r:pull_request`），默认 `aud` 是 `https://github.com/<owner>`。端点：`/oidc/.well-known/openid-configuration`、`/oidc/jwks`、`/oidc/token`。单测：oidc 包 8 个（发现文档、用公布的公钥集独立验签、claim 逐项、`sub` 各形式、request token 的各种拒绝：缺失/乱码/改签名/改负载/别的进程/过期/没有的 run/已结束的 run、audience 过长或带换行、密钥封存且重启后不变、主密钥换了就换新的、无 vault 时每进程一把、要求 https）、执行器 2 个（哪些 job 拿到环境、fork 不给）；真 runner 的测试（`TestRealRunnerAJobFetchesItsIDToken`：step 用 curl 按 toolkit 的方式取 token，claim 正确）；经隧道取到了发现文档和公钥集。**没做/没测**：没有任何云厂商接受过这种 token（需要用户在某个云账号里建信任）；`environment` 和带它的 `sub`、`head_ref`/`base_ref`、`repository_id`、`job_workflow_sha`、自定义 `sub` 模板；`actions/core` 的 `getIDToken` 没在真机上跑过（那些仓库没有 workflow 可加）；`ghrunner`（GitHub 触发路径）不需要这个，GitHub 自己签
- 🟡 真实的 `GITHUB_TOKEN`（2026-11-02，**未提交**，服务端 App 模式）：每个 job 实例一个 installation token，只对 run 的仓库，权限取 job 的 `permissions:`、否则 workflow 的、否则默认（`internal/server/execute/permissions.go`，`github.App.RunToken`）；`-job-token` 默认开。**设计取舍，先按最稳妥的做，等你拍板**：①默认只读 contents+metadata（GitHub 自己的默认可能读写，取决于仓库设置，所以依赖默认写权限的 workflow 需要显式写 `permissions:`）；②fork 的 PR 一个 token 都不给（GitHub 给只读的；我们跟"不给密钥"一致）；③App 没有 job 要的权限时降级为默认权限并在日志警告，而不是让 job 失败；④普通 token（开发用）绝不传给 job。单测：权限表（映射、空映射、read-all、write-all、job 覆盖 workflow、去掉 id-token）、客户端 2 个（只问一个仓库+指定权限、422 与无 installation）、执行器 4 个（有 token、fork/无 installation/无 minter 没有、拒绝时降级并警告、全失败时无 token 并警告、日志里没有 token）。**真 GitHub 上测过 GitHub 一侧**（2026-11-02，`TestRealAppTokens`，用 `RACCOONCI_TEST_APP_*` 开启）：只对一个仓库的 token 能读它（200）、读同一安装下别的私有仓库 404；只读 token 写 status 403；`statuses: write` 的能写（201）、读 contents 被拒；App 没有的权限（packages）真的返回 422，所以降级路径是对的；吊销后失效（GitHub 要等一小会儿才生效，测试轮询）。一个真实 run 里要 `packages: write` 的 job 拿到降级 token 并有警告。用真正的 `run-instance`（`TestRealRunnerGivesAJobItsGithubTokenAndMasksIt`，`RACCOONCI_TEST_BIN`）也测了：step 里 `github.token` 和 `secrets.GITHUB_TOKEN` 是同一个值，打印出来是 `***`，日志里没有原值。**没测到的**：InstaBox 机器上的 step 真的拿真 token 去调 API——手边的 gh token 没有 `workflow` 权限，加不了 workflow 文件，把 run 里存的文本换成探针也没用（**发现：机器从源码包里读 workflow 文件，不是用库里保存的文本，两者在同一提交上一致所以平时没事**）。要测的话需要你运行 `gh auth refresh -s workflow`（或在 `bench-lint-docs` 里加一个探针 workflow）。没做：`id-token`（OIDC，仍是 B.3 的未决项）；（job 一结束就吊销它的 token：`App.RevokeToken` = `DELETE /installation/token`，用 token 自己认证，已失效的 401 算成功，取消时也会吊销，失败只记日志、一小时后自己过期；单测：客户端 1 个、执行器断言吊销了正确的 token）
- ⬜ job summary 的展示与 annotations 持久化：`runner.Result.Summary` 收集了 `$GITHUB_STEP_SUMMARY` 的内容（上限 1 MiB），但没有任何地方读它，本机运行也不打印。用户在自有引擎下看不到 summary

**搁置：单机之外才需要**：（artifact 元数据持久化和 retention 清理服务端已做，`raccoonci run` 本机仍不做）整体 sha256 校验、cache 按分支隔离、多进程写同一个 index、下载链接单独的密钥、本机运行的按 job token。它们都只在多人共用一台 controller 或长期运行的服务里才会出问题；现在 `raccoonci run` 是单次运行、单进程。做长期服务（C.1）时再捡起来。

**B.3 的未决项（需要你定）**：OIDC 和真实的 `GITHUB_TOKEN` 是 GHA 兼容性缺口，用 OIDC 登录云厂商的 workflow 现在会失败。做法取决于谁来签发 token（自有引擎自己签，还是接外部身份），还没定，所以没有排期；定了以后需要服务端（C.1）。

## B.4 job 类型缺口（现在会给出明确报错，不会静默）

- ✅ 容器 job（`container:`）和 `services:`：`internal/docker`、`pkg/runner/containers.go`。用真实 Docker 实测（`RACCOONCI_TEST_DOCKER=1`）：busybox 和 debian:bookworm-slim 作 job 容器、redis 作 service（健康检查、容器内按名字访问、宿主机 job 通过发布的端口访问）、容器内的本地 Node action、job 失败/镜像不存在/service 不健康时的清理，结束后没有残留容器和网络
- ✅ 容器 job 里的 artifact 和 cache 服务：job 网络建好后，服务在这张网络的网关地址（宿主机在这张网络上的地址）上再开一个监听，只接受这张网络子网内的连接；签发的上传/下载链接按请求进来的那个地址生成（`blob.WithBaseURL`），鉴权不变。实测：容器里 `upload-artifact@v4`（12 MB）+ `actions/cache@v4`（v1 和 v2 都测了），之后容器 job 和宿主机 job 各自下载并校验通过；其他 docker 网络和宿主机 eth0 地址连不上，同一网络里没有 token 返回 401；跑完后监听、网络、容器都没有残留。rootless docker 下不支持（网络不在宿主机上），会给出警告
- ✅ 取消容器里的步骤：步骤经 `sh -c 'echo $$ >pidfile; exec …'` 启动（docker 把每个 exec 放进自己的进程组，pid 即 pgid），取消时用另一次 `docker exec` 向容器内的进程组发 SIGINT，宽限期后发 SIGKILL。实测：步骤里的 `trap INT` 会执行，后台子进程随组退出；去掉这处修复后该测试会失败
- ✅ `docker login` 对真实私有仓库：本地 `registry:2` + htpasswd 认证，job 用 `credentials:` 拉取成功；密码错误则 job 失败且日志里没有密码；宿主机自己的 docker 配置没有被登录状态污染
- ✅ job 结束后 chown：断言容器里 root 写的文件归还给当前用户；把 chown 关掉后断言会失败
- ✅ Alpine（musl）镜像的 JS action：job 容器是 musl 时用 `unofficial-builds.nodejs.org` 的 musl 版 Node（同样校验 SHA-256，装在 `node/musl/` 下，随 `node/` 一起挂进容器）。musl 版动态链接 libstdc++，Alpine 没有，所以 Node 起不来时在容器里以 root 执行 `apk add libstdc++`（只影响这个 job 的容器，步骤日志有 notice）。实测真实 `upload-artifact@v4`/`download-artifact@v4` 在 Alpine 容器里跑通。`actions/cache` 在裸 Alpine 里会因 busybox 的 tar 失败，与 GitHub 上相同，镜像里需要 `apk add tar`
- ⬜ 容器 job 剩余：宿主机防火墙若拦截 INPUT（如 ufw 默认 deny），容器连网关会失败，目前没有检测；非 root 用户的镜像（`--user`）；被强杀的进程留下的孤儿容器没有清扫（容器带标签 `raccoonci=1`）；Alpine 的 `apk add` 需要容器能访问 Alpine 镜像源，离线或非 root 的 Alpine 镜像要自己预装 libstdc++；没有 `/bin/sh` 的镜像（distroless）上取消仍然只会杀 docker 客户端
- ✅ Docker action（2026-10-29，`pkg/runner/docker_action.go`）：`uses: docker://image`（`with.entrypoint`、`with.args` 按命令行规则切分，不展开）和 `runs.using: docker`（`image: docker://…` 先拉取，`image: Dockerfile` 用 action 目录 `docker build`，每个 job 只建一次；`runs.entrypoint`/`args`/`env`，args 和 env 里可以用 `inputs`；`pre-entrypoint`/`post-entrypoint` 带 pre/post-if）。做法照 GitHub 的 runner：`docker run --rm`，工作区、temp、工具缓存、文件命令目录、event 目录和一个 home 分别挂到 `/github/workspace`、`/github/runner_temp`、`/opt/hostedtoolcache`、`/github/file_commands`、`/github/workflow`、`/github/home`，`GITHUB_*`/`RUNNER_*` 里的路径变量改写成容器里的路径，不传本机环境和 PATH，有 job 网络就加入，挂 docker.sock；输出走和别的步骤一样的路径，所以 `::set-output` 类命令和 `$GITHUB_OUTPUT`/`$GITHUB_ENV` 都能用；取消时先 SIGINT（docker 客户端转发给容器），宽限期后 `docker kill`。结束后用同一个镜像 `chown` 把容器里 root 写的文件还给当前用户（镜像没有 chown 就算了）。真实验证：`actions/hello-world-docker-action@v2`（Dockerfile 构建，输出被后面的步骤读到）、`docker://alpine`，以及一个带 pre/post/args/env 的本地 Dockerfile action（`TestRealDockerActions`，需要 `RACCOONCI_TEST_DOCKER=1`）。**注意**：`runs.args` 现在也传给 pre/post entrypoint（凭记忆照 runner 的实现，没找到文档确认）；pre 只对远程 action 执行（和 JS 的 pre 一样）；不支持 `--user`/job 容器 `options:` 对 action 的影响；机器上 docker 要可用（InstaBox 的 universal 模板有）
- ⬜ 可复用 workflow（`jobs.<id>.uses`、`workflow_call`）
- ⬜ problem matcher（目前输出原样记录，不生成 annotation）

## B.5 workflow 语义缺口

- ✅ `on:` 触发匹配（2026-10-29，`pkg/trigger`）：事件写法三种（字符串、列表、映射）；`push`/`pull_request`/`pull_request_target` 的 `branches`、`branches-ignore`、`tags`、`tags-ignore`、`paths`、`paths-ignore` 按 GitHub 的规则——过滤用的是 GitHub 自己的 glob（`*` 不跨 `/`，`**` 跨，`**/x` 也匹配根目录的 x，`?` `+` 作用于前一个字符，`[]` 字符集，`\` 转义），列表里 `!` 取反且顺序有意义（后面的正向模式可以重新选中），只有取反模式的列表报错；只写了 branches 时推 tag 不触发、只写了 tags 时推分支不触发；PR 的 branches 过滤看目标分支；tag 推送不看 paths；`paths` 与 `paths-ignore`、`branches` 与 `branches-ignore`、`tags` 与 `tags-ignore` 同时出现是错误；`types`（`pull_request` 默认 opened/synchronize/reopened，其它事件不写 types 就全部）；`schedule` 校验 cron 是 5 段并按字符串匹配触发的那条；`workflow_dispatch` 的 `inputs` 解析（默认值、required、choice 的 options、boolean/number 的类型转换、不认识的输入报错——`run -event workflow_dispatch -input k=v` 现在走这条，`inputs.x` 因此是带类型的，和 GitHub 一致）。文档里的过滤示例做成了测试，80 个真实 workflow 全部能解析。命令行：`raccoonci match -event E [-ref R -base-ref B -before REV -file F -action A -cron C] workflow.yml`（退出码 1 表示不触发；变更文件默认取 git 的 `HEAD~1..HEAD`，PR 取 `base...HEAD`，取不到就是"未知"，paths 过滤不拦），`run -only-if-triggered` 同样判断后才跑（默认 `run` 仍然假设触发，因为是用户明确要跑）。`serve` 路径的 push/PR 提前启动也用它（A.1）。**还没做**：`workflow_call`（可复用 workflow，见 B.4）；`workflow_run`、`merge_group` 的 `workflows`/`branches` 过滤；push 事件按 GitHub 的"最多 300 个文件就总是触发"；`schedule` 的 cron 到点计算（自有引擎的服务端要做；`serve` 路径已有 `cron.go`）；把 `match` 的事件从 webhook 载荷里取（服务端，C.1）
- 🟡 `concurrency`（2026-11-02，**未提交**，服务端 `execute/concurrency.go`）：workflow 级 `concurrency:` 按仓库+组名（不分大小写）生效，组名和 `cancel-in-progress` 都可以是表达式（`github`/`inputs`/`vars`）；组里一个在跑、一个在等，新来的顶掉等待者（等待者以 cancelled 结束，日志写明），`cancel-in-progress` 为真时立刻取消在跑的；等待中被取消会让出位置；源码在等待之后才取。单测 7 个（排队等待、只留最新的等待者、cancel-in-progress、它是表达式且为假时只等、不同组并行、取消等待者后组空出、组名表达式出错），`-race` 重复 5 次稳定。**job 级 `concurrency:`（2026-11-02，**未提交**）**：同一套组机制（`holder` 带自己的取消函数，run 和 job 共用）；job 的组表达式能用 github/inputs/vars/needs/strategy/matrix；进组在 StartJob 之前，等待的 job 没有行、check run 和机器；`cancel-in-progress` 只取消组里的那个 job（它以 cancelled 结束，日志写明），不取消它的整个 run；被顶掉的等待 job 结论为 cancelled；workflow 级与 job 级的同名组是两个组（键带层级前缀）；等待 job 的 run 仍占着 `MaxRuns` 名额（run 已在执行，不让出）。单测 7 个（跨 run 等待且无行、只留最新的等待者、cancel-in-progress 只取消 job、同一 matrix 同组轮流、每个值一个组并行、两层同名是两个组、组名表达式出错）；变异检查：去掉 job 级进组时 4 个测试失败。**取舍/没做**：一个被取消的 job 让所在 run 的结论是什么，由引擎按 needs 的规则决定，没有单独调整；**没做**：`concurrency` 在 `raccoonci run`（本地引擎）里没意义所以不做；`raccoonci run`（本地引擎）里的 `concurrency` 没有意义所以没做；状态只在内存里（单实例）；（等待组的 run 不再占 `MaxRuns` 名额：等待时让出、轮到时再拿，让出时唤醒执行器去领别的 run；测试 `MaxRuns=2`，一个在跑一个在等时另一个组的 run 照常跑完，变异检查：去掉让出这个测试会失败）；没在真机上测
- ⬜ `permissions` 和 `environment`（含保护规则、环境级 secrets）
- ⬜ `run-name`
- 🟡 `github` 上下文：`run` 命令从 git 生成了一部分字段，真实的 run_id、事件载荷等要服务端提供（C.1）
- 🟡 `hashFiles`：自己写的 glob 实现，**没有逐项对照 `@actions/glob`**，边界行为可能有差异

## B.6 按需执行（Raccoon CI 自己的扩展，不是 GitHub 的语义）

灵感来自 Vercel / Turborepo：改了后端就不跑前端的 job。`internal/affected` + `engine.Options.Unaffected`。这是自有 trigger 才能做的加速；A 路径下怎么用见 A.4 的"跑了但实际没跑"。

- ✅ job 级（2026-10-29）：`raccoonci affected [-event E -file F -before REV ...] workflow.yml` 说明每个 job 为什么跑、为什么不跑；`raccoonci run -affected` 把不受影响的 job 直接略过（打印 "not run: …"，最后汇总里标 `-`），依赖它的 job 看到的 `needs.<id>.result` 是 `success`。判断依据保守：只有**能确定** job 的输入范围，并且变更文件都落在范围外，才会略过；其余一律照跑。范围来源：①job 的 `defaults.run.working-directory` 或每个 `run:` 步骤自己的 `working-directory`，加上 `uses: ./本地action` 的目录（任何一个 `run:` 在仓库根目录跑、目录只在运行时才知道，都当成"整个仓库"）；②包依赖图（下一条）；③可选的 `.raccoonci/affected.yml`（`ignore` 不影响任何目录型 job 的文件、`global` 影响所有 job 的文件、`jobs.<id>` 显式给出一个 job 的依赖，用 GitHub 的 glob；放在 workflow 文件之外，因为 GitHub 不认识 workflow 里的陌生键）。变更文件不属于任何包或 job 目录、又不在 `ignore`/默认忽略（`**.md`、LICENSE、.gitignore、.editorconfig）里 → 影响所有目录型 job；workflow 文件本身、`.github/actions/**`、配置文件变了 → 全部要跑；变更文件未知 → 全部要跑。**有输出（`outputs:`）或上传 artifact 的 job 永远不略过**（略过会让下游缺东西）。
- ✅ 依赖图级（2026-10-29）：工作区包的发现和依赖——Go（`go.work` 的 `use`，模块之间的 `require` 和指向本地目录的 `replace`）、npm/yarn（`package.json` 的 `workspaces`）、pnpm（`pnpm-workspace.yaml`，支持 `!` 排除）、Cargo（`[workspace] members/exclude`，`path` 依赖，以及与成员同名的依赖——按"同名即本地"保守处理，因为常常靠 `[patch]` 指过去）。job 的范围 = 它工作目录里的包 + 这些包依赖的包（传递）；所以改了底层库，用它的上层 job 会被判为受影响，不相干的包不会。在真实仓库上看过图：vite（287 个 pnpm 工作区包，含 playground 和测试夹具）、tokio（10 个 crate，dev-dependencies 形成环，处理没问题）。
- ⬜ 还没做：①很多项目的 job 不写 `working-directory`，而是用 `cargo test -p x`、`pnpm --filter x`、`npm --workspace`、`go test ./backend/...`、`turbo run`、`nx affected` 这类命令选包——要从 `run:` 里读出这些选择器（按 catalog 描述每个工具的选包参数）才能让 tokio 这类仓库的 job 受益；②矩阵里用 `matrix.xxx` 作为 working-directory 的 job（现在当"整个仓库"）；③内容哈希：Turborepo 是"输入哈希没变就重放上次的结果"，我们现在是"看 diff"。哈希版本不依赖基线提交，并且能重放 outputs 和 artifact（让有输出的 job 也能略过），需要服务端记录每个 job 的"输入哈希 → 上次成功的结果"（C.1）；④go 单模块内的包级依赖（要 `go list`）；⑤`on: workflow_run`/多个 workflow 之间的联动；⑥JS 的 `tsconfig` 路径引用、Python 工作区（uv）。

## B.7 MVP 之后

- ⬜ AI agent：失败 run 的分诊与自动修复（已提过的核心卖点）
- ⬜ 从 Travis / CircleCI / Azure Pipelines 迁移（MVP 只做 GitHub Actions 格式）
- ⬜ 原生配置格式

## B.8 工程与风险

- ✅ 用 Raccoon CI 自己跑自己的 `make check`（dogfood）：`.github/workflows/ci.yml`（checkout、`setup-go` 用 `go-version-file`、gofmt、`make check`、`make build`、`upload-artifact`），在本机 1 分钟左右跑通。这次暴露并修复了一个真问题：缓存/artifact 服务暂存分块默认放在 `/tmp`，本机的 `/tmp` 是 2 GB 的 tmpfs，保存一份 Go 构建缓存（约 750 MB 压缩）时写满，服务端回 500，`actions/cache` 只报 “responded with 500” 和一串 EBADF。现在暂存目录放在缓存/artifact 仓库旁边的持久盘上（`azblob.NewSpoolDir`，每个进程一个私有子目录，超过一天的残留自动清理）
- 🟡 兼容性语料：对 13 个真实开源项目的 90 个 workflow 文件跑了 `plan`，全部解析并展开通过。真正跑起来的有 `spf13/cobra` 的 `test-unix`（8 个 Go 版本 × ubuntu 全部通过，8 个 macOS 实例按设计明确失败）。**还没跑**：commander.js（Node，actions 用 commit SHA 固定）、click（Python + uv + tox）、express、需要 Rust 的项目（没有 Rust 安装器，语料里约 60 个实例需要它）
- ⬜ 已知：同一台机器第二次跑时，`setup-go` 还原缓存会因为 `~/go/pkg/mod` 里已有的只读文件而报 “File exists”（tar 退出码 2，只是警告，job 照常成功）。这是长期使用的自托管机器上 `setup-go` 的已知行为，GitHub 的全新虚拟机不会遇到；对“用户自己的机器”这条路线会常见，需要想办法（例如每次 job 用干净的 HOME，或者还原前清理）
- ✅ 服务端 5xx 的原因以前不会出现在任何日志里：现在 `raccoonci run` 会把 artifact/cache 服务的 5xx 连同原因打到 stderr（`local.LogServerErrors`，只记路径，不记签名）。靠它确认了 cobra 矩阵里 `1.22.x` 的缓存提交失败是 `no space left on device`（`/data` 最低剩 366 MiB）
- ✅ 缓存大小上限和淘汰：`-cache-limit-gb`（默认 10，0 为不限），超过时淘汰最久没被命中的条目；比上限大的条目直接拒绝；提交前检查磁盘剩余空间，不够就先淘汰，淘汰完还不够返回 507。v1、v2 两条协议都覆盖，单测包含淘汰顺序、索引持久化、磁盘不足
- ⬜ 缓存提交时仍要在仓库目录里再写一份完整副本（暂存 + 仓库，瞬时 2 倍）：`Dir` 仓库可以直接把暂存文件改名进仓库，省掉那份拷贝；淘汰只在单进程内有效，多个 `raccoonci` 进程共用同一个缓存目录时各自的上限互相看不见
- ✅ 引擎：不可用 OS 的实例不再占并发槽位排队，立即失败（实测 cobra 的 8 个 macOS 实例一开始就全部报错）
- 🟡 硬编码原则：evolving 的名单都已进 catalog；GitHub 协议本身（字段名、workflow 命令名、上限）和下载地址有意留在代码里，原因见 `CLAUDE.md`

---

