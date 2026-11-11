# Testing and platform notes / 测试与平台说明

## Reproduce the checks / 复现检查

Use the Go version in `go.mod`, a working Git executable, and the platform's
native build prerequisites. Run from the repository root:

使用 `go.mod` 指定的 Go 版本、本机 Git 和平台构建依赖，在仓库根目录执行：

```sh
go test -count=1 ./...
go vet -tags "vivahead litheaudit" ./...
go test -count=1 -tags "vivahead litheaudit" ./internal/ui
```

`vivahead` enables Viva's offscreen UI tests. `litheaudit` includes the
external-issue regression scenarios. Neither command launches the app window.
The renderer still needs its platform rendering backend. Git fixtures use
temporary repositories and isolated configuration, including local temporary
remotes; they do not authenticate to public Git hosting.

`vivahead` 启用 Viva 离屏 UI 测试，`litheaudit` 加入外部 issue 回归场景。
不会启动应用窗口，但仍需要平台渲染后端。测试使用临时仓库、隔离配置和本地临时远端，
不测试公网 Git 托管服务的登录过程。

The Subversion adapter's tests need an `svn` client on `PATH`, plus `svnadmin` for
the `file://` fixtures they create; `VIFF_SVN` and `VIFF_SVNADMIN` override the
lookup, which is what a Windows install outside `PATH` needs. Without a client
they skip, so a machine without Subversion proves nothing about that adapter: the
Linux CI shard installs `subversion` and fails if the package skips.

Subversion 适配器的测试需要 `PATH` 上有 `svn`，以及为 `file://` 仓库服务的 `svnadmin`；
`VIFF_SVN` 与 `VIFF_SVNADMIN` 可覆盖查找结果（Windows 装在 `PATH` 之外时用它们）。
没有 svn 客户端时这些用例会跳过，所以只跑跳过等于没有验证这一层：CI 的 Linux 分片
会安装 `subversion`，并在该包出现跳过时报失败。

For screenshots on macOS / macOS 保存截图：

```sh
VIFF_SHOTS="$PWD/.viff-test/shots" VIFF_SHOTS_BY_TEST=1 \
  go test -count=1 -tags "vivahead litheaudit" ./internal/ui
```

On Windows PowerShell / Windows PowerShell：

```powershell
$env:VIFF_SHOTS = "$PWD/.viff-test/shots"
$env:VIFF_SHOTS_BY_TEST = "1"
go test -count=1 -tags "vivahead litheaudit" ./internal/ui
```

## Pre-publication acceptance / 发布前验收

### Windows native file watching / Windows 原生文件监听

Compare the same checkout and Git executable against the previous polling build.
After the initial status settles, count ordinary `status` calls, Git process-tree
CPU, Viff CPU and process handles over five minutes in each stable window state:
foreground, unfocused and minimized. Record focus recovery separately. On a
disposable checkout, generate a burst under a Git ignored directory, then edit
a tracked file there and check that it is still reported. Measure command counts
for the burst separately from the idle interval. See [ADR 0049](decisions/0049-windows-native-file-watching.md).

前后两版使用同一工作树和 Git 可执行文件。首轮状态稳定后，在前台、失焦和最小化三种
状态下各观察五分钟，分别记录普通 `status` 次数、Git 进程树 CPU、Viff CPU 和进程句柄数。
恢复焦点单独统计。在隔离工作树的 Git 忽略目录中批量生成文件，再编辑该目录中已跟踪的
文件并验证界面能更新；批量事件和空闲窗口分别统计。见 [ADR 0049](decisions/0049-windows-native-file-watching.md)。

On Windows, run `go test -count=1 ./internal/platform ./internal/gitcli ./internal/app`
for file creation, edits, deletion and rename, shared and per-worktree metadata,
ignored and tracked paths, native buffer loss, rearming, cancellation and polling
selection. The suspend detector is exercised with synthetic elapsed and awake
clock samples; desktop focus and idle CPU are measured with the actual executable.

Windows 上运行 `go test -count=1 ./internal/platform ./internal/gitcli ./internal/app`，
覆盖创建、编辑、删除、重命名，共享及工作树私有元数据，忽略及已跟踪路径，
原生缓冲丢失、恢复订阅、取消和轮询回退。休眠检测使用模拟的总经过时间和系统运行时间；
桌面焦点与空闲 CPU 使用实际应用测量。

### LFS attribute discovery / LFS 属性文件发现

Use the same disposable checkout for both builds. Add tracked and visible
untracked nested `.gitattributes` files and a generated directory tree.
Record the first discovery separately from idle refreshes; classify Git
commands by subcommand and whether the pathspec is `:(glob)**/.gitattributes`.
For those commands, record count, elapsed time and Git process-tree CPU.
Count completed ordinary `status` calls separately. See [ADR 0048](decisions/0048-lfs-attribute-discovery-cache.md).

两版使用同一隔离工作树，放入已跟踪和可见的未跟踪嵌套 `.gitattributes`，以及一棵生成目录树。
首次发现和空闲刷新分开统计；按 Git 子命令及 `:(glob)**/.gitattributes` 路径规范识别发现命令，
记录次数、墙钟耗时及 Git 进程树 CPU 时间；单独统计完成的普通 `status`。
见 [ADR 0048](decisions/0048-lfs-attribute-discovery-cache.md)。

The controlled test advances the clock through five minutes of ordinary and
metadata refreshes. It expects one startup discovery and no further discovery
or LFS file listing. Other isolated cases cover ignored paths, index replacement,
hidden intent-to-add, equal-size edits, switching linked worktrees, failure
and cancellation. The parse cache has independent entry, pattern and byte bounds.

受控测试推进时钟覆盖五分钟的普通和元数据刷新，验证仅在首次启动时发现一次，
后续不再发现属性文件或枚举 LFS 文件。其他隔离用例覆盖忽略、索引替换、
隐藏的 intent-to-add、等长编辑、切换关联工作树、失败和取消。
解析缓存分别限制条目、模式与文本字节数。

### On-demand LFS statistics / 按需 LFS 文件统计

Fork issue [#2](https://github.com/muchenhen/viff/issues/2) separates background
rule detection from explicit file statistics ([ADR 0047](decisions/0047-lfs-on-demand-statistics.md)).
The regression suite uses isolated temporary repositories and channel-controlled
listings. It covers automatic refreshes through five simulated minutes, status
publication during a blocked query, duplicate requests, successful zero,
preserved results on failure, restaging the same path with another blob,
HEAD changes, switch/close/reopen, and adapters without LFS. Dark/light headless
tests use Tab/Enter to request statistics and verify count, time, failure,
missing-dependency and clipboard behavior.

Fork 的 [#2](https://github.com/muchenhen/viff/issues/2) 把后台规则检测与主动文件统计分开
（[ADR 0047](decisions/0047-lfs-on-demand-statistics.md)）。回归使用隔离临时仓库和
channel 控制的枚举，覆盖模拟五分钟内的自动刷新、阻塞查询期间的状态发布、重复请求、
成功零值、失败保留结果、同一路径重新暂存不同 blob、HEAD 变化、切换/关闭/重开与
不支持 LFS 的适配器。明暗主题无头测试用 Tab/Enter 主动统计，并验证数量、时间、
失败、缺依赖和剪贴板行为。

Cancellation tests check native Windows descendants and the transport-side
POSIX wrapper independently. The Linux test binary was cross-compiled and
executed three times in the existing Docker Desktop WSL kernel, without Git
or a network: three process levels, normal completion, missing exit proof,
process-inspection failure, and five completion/cancellation races per run.
The wrapper requires a valid exit proof before releasing a worktree reservation.

取消测试分别检查原生 Windows 后代进程和传输另一侧的 POSIX wrapper。交叉编译的
Linux 测试程序在现有 Docker Desktop WSL 内核运行三轮，不需要 Git 或网络：
覆盖三级进程、正常完成、缺少退出确认、进程检查失败及每轮五次完成/取消竞态。
收到有效退出确认后才释放工作树查询名额。

Measurements on Windows 11 Pro 10.0.26200 x64, 16 logical processors,
Go 1.27.3, Viva 0.2.4, Git 2.45.1.windows.1 and Git LFS 3.5.1 compared
the prior #1 binary from `f9ce062` with the reviewed #2 source. Both binaries
used separate Viff settings/log directories and the same Job Object Git meter.
Refresh timings use the repo.refresh duration after discovery. CPU totals cover complete Git process trees for commands that finished inside
the observation window, excluding fixture creation and Viff itself. Each
comparison below was run once; ordinary file changes and the existing polling
watcher remained enabled in the authorized real UE checkout.

测量平台为 Windows 11 Pro 10.0.26200 x64、16 个逻辑核心、Go 1.27.3、
Viva 0.2.4、Git 2.45.1.windows.1、Git LFS 3.5.1。对照 #1 的 `f9ce062`
构建与经审查的 #2 源码构建。两版使用独立 Viff 设置/日志目录和同一 Job Object
Git 计量程序。刷新耗时取发现仓库后的 repo.refresh 时长。CPU 为观察窗口内已完成命令的完整 Git 进程树累计，排除夹具创建和
Viff 本体。以下每版各测一次；授权的真实 UE 工作树保留普通文件变化与既有轮询监听。

| Real checkout, first 300 seconds / 真实工作树前 300 秒 | Before / 改前 | After / 改后 |
| --- | ---: | ---: |
| First refresh duration / 首轮刷新耗时 | 100.731 s | 7.394 s |
| Automatic `lfs ls-files` commands, including format attempts / 自动枚举命令，含格式探测 | 6 (3 queries / 次统计) | 0 |
| Completed Git commands / 完成的 Git 命令 | 53 | 76 |
| Git process-tree CPU / Git 进程树 CPU | 315.734 s | 178.625 s |
| LFS process-tree CPU, including version probe / LFS 进程树 CPU，含版本探测 | 217.375 s | 0.391 s |

The completed-command CPU total decreased by 43.43%. The new Viff process used
6.219 CPU seconds over 300.065 seconds, or approximately 0.130% of total
16-core capacity. Status refreshes continue more often after removing the
long enumeration; attribute discovery and polling remain the next performance
items (#3/#4).

已完成命令的累计 CPU 时间下降 43.43%。新版 Viff 本体在 300.065 秒内使用
6.219 秒 CPU，折合 16 核整机容量约 0.130%。移除长时间枚举后，状态能更频繁地
完成刷新；属性发现与轮询继续由 #3/#4 优化。

The reproducible synthetic checkout contains 4,096 committed LFS pointers
sharing one blob and 8,192 ordinary tracked files. Both versions open this
same checkout and remain idle for 30 seconds, with the normal watcher enabled.

可复现的合成工作树包含 4,096 个已提交 LFS 指针（共用一个 blob）和 8,192 个
普通跟踪文件。两版打开同一个工作树，启用正常监听并空闲观察 30 秒。

| Synthetic checkout / 合成工作树 | Before / 改前 | After / 改后 |
| --- | ---: | ---: |
| First refresh duration / 首轮刷新耗时 | 7.961 s | 1.363 s |
| Automatic `lfs ls-files` commands / 自动枚举命令 | 2 | 0 |
| Git process-tree CPU / Git 进程树 CPU | 8.797 s | 1.453 s |
| LFS process-tree CPU / LFS 进程树 CPU | 7.734 s | 0.375 s |

An application-layer query against the same real checkout returned 21,366
files in 95.828 seconds; a regular status refresh completed after 7.804 seconds
while the listing was still running. Five requests (one plus four duplicates)
produced one enumeration: two LFS commands for the rejected `-z` attempt and
the successful JSON fallback. The full LFS listing process tree used 92.078 CPU
seconds. Its files were counted locally and only aggregate results were saved.

同一真实工作树的应用层主动查询在 95.828 秒内返回 21,366 个文件；枚举仍在运行时，
普通状态在 7.804 秒完成刷新。一个请求和四次重复请求只产生一次枚举：拒绝 `-z`
的尝试和成功的 JSON 回退共两条 LFS 命令，完整枚举进程树使用 92.078 秒 CPU。
文件在本地计数，验证记录只保存聚合结果。

### Coalesced refresh scope / 合并刷新的范围

`go test -count=1 -run '^TestRefresh' ./internal/app` covers fork issue #1.
Channel-controlled status queries verify that overlapping ordinary requests
produce one ordinary follow-up and reuse the freshly published LFS snapshot.
Metadata requests are OR-ed into the pending scope; the active pass's scope
does not carry forward. Each pass consumes the pending state before starting,
so requests during that pass can form one further pending refresh. Failure,
cancellation, repository removal, switching, and close/reopen scenarios check
that pending requests and late results stay within their repository lifetime.
Lifetime assertions use the UI-thread query sequence and busy state, so a stale
callback cannot hide a newly started refresh behind an unscheduled goroutine.
Queued metadata also invalidates the loaded log before the follow-up queries:
local shallow-clone tests fetch with `--unshallow` and `--deepen=1`, keeping
HEAD and refs unchanged, and check that the fetched parent appears in the log.
At this stage the LFS attribute fingerprint and 30-second recheck rules still
applied; the current discovery policy is documented in [ADR 0048](decisions/0048-lfs-attribute-discovery-cache.md).

`go test -count=1 -run '^TestRefresh' ./internal/app` 覆盖 fork issue #1。
测试通过 channel 控制状态查询，确认重叠的普通请求只产生一次普通补刷，并复用刚发布的
LFS 快照。待处理范围按元数据请求取 OR，不继承在途刷新的范围；每轮开始前消费待处理
状态，该轮期间到达的新请求仍可合并为下一次补刷。失败、取消、仓库消失、切换及关闭后
重开场景检查待处理请求和迟到结果始终属于原仓库生命周期。生命周期断言检查 UI 线程
同步维护的查询序号和刷新状态，避免新刷新的 goroutine 尚未调度而漏报。排队的元数据
请求还会在补刷查询前使已加载日志失效：本地浅克隆测试分别执行 `--unshallow` 和
`--deepen=1`，保持 HEAD、refs 不变，检查新取得的父提交出现在日志中。
该阶段仍使用 LFS 原有的属性指纹和 30 秒重查规则；当前策略见 [ADR 0048](decisions/0048-lfs-attribute-discovery-cache.md)。

The November 11, 2026 comparison used Windows 11 Pro x64, Go 1.27.3,
Git 2.45.1.windows.1, and Git LFS 3.5.1. Both versions opened the same
isolated fixture shape: 4,096 committed LFS pointers, a status result held for
31 seconds, and ten ordinary requests delivered at 30 seconds. A temporary
`go test -overlay` harness ran `TestRefreshSlowAcceptance` against `cc76616`
and the fix. Required and optional services share one metered runner; the
harness asserts two stash and two worktree queries in each run. A Windows Job
Object around each Git command in the measured open/refresh workload accounted
for its entire process tree, including git-lfs and its Git children; CPU time
excludes fixture setup and the artificial wait. Each version was measured once.
The fixture leaves `Workspace.Watch` unset and injects one burst of requests;
it stops after the two refresh passes settle. Its pointer files share one blob,
and it contains no untracked-file workload.

2026 年 11 月 11 日的对照环境为 Windows 11 Pro x64、Go 1.27.3、
Git 2.45.1.windows.1、Git LFS 3.5.1。两版使用同样的隔离夹具结构：
4,096 个已提交的 LFS 指针、延迟 31 秒返回的状态结果，以及第 30 秒到达的十个普通请求。
临时 `go test -overlay` 夹具通过 `TestRefreshSlowAcceptance` 分别运行 `cc76616`
和修复版。必备接口和可选能力共用一个计量 Runner，夹具断言每版各包含两条 stash、
两条 worktree 查询。被测打开与刷新流程的每条 Git 命令置于 Windows Job Object 内，
统计完整进程树，含 git-lfs 及其 Git 子进程；CPU 时间不含夹具搭建和人为等待。
每版各测一次。夹具未设置 `Workspace.Watch`，只注入一批请求，并在两轮刷新完成后
结束。所有指针文件共用一个 blob，未构造未跟踪文件负载。

| Metric / 指标 | Before / 修复前 | After / 修复后 |
| --- | --- | --- |
| Refresh scopes / 刷新范围 | `true, true` | `true, false` |
| Status queries / 状态查询 | 2 | 2 |
| Git commands / Git 命令 | 24 | 21 |
| Stash queries / stash 查询 | 2 | 2 |
| Worktree queries / worktree 查询 | 2 | 2 |
| LFS commands / LFS 命令 | 5 | 3 |
| LFS listing attempts / LFS 枚举命令 | 4 | 2 |
| Git process-tree CPU time / Git 进程树 CPU 时间 | 18,734 ms | 11,203 ms |
| LFS process-tree CPU time (included above) / LFS 进程树 CPU 时间（已含在上项） | 14,484 ms | 6,938 ms |
| Follow-up duration / 补刷耗时 | 15,580 ms | 2,778 ms |

The old version started a second metadata refresh about 34.0 seconds after
the first; the fixed version completed one metadata pass followed by an
ordinary pass. LFS 3.5.1 rejects `ls-files -z`, so each enumeration includes
the existing newline-format fallback: two listing commands, not two scans.
The fixture measures coalescing; actual checkout size, cache state, and disk
load determine timings in a real repository.

旧版在首轮开始约 34.0 秒后启动第二次元数据刷新；修复版完成一次元数据刷新后按普通
范围补刷。LFS 3.5.1 拒绝 `ls-files -z`，每次枚举包含已有的换行格式回退，因此会有
两条枚举命令。此夹具验证合并行为；实际仓库耗时取决于规模、缓存和磁盘负载。

Using the unrounded counters, `(before - after) / before` gives a 40.20%
reduction in accumulated Git process-tree CPU time and 52.10% for the included
LFS trees over these two passes. These percentages describe the synthetic
workload's CPU time; Viff's own CPU use and whole-machine utilization need
separate measurements. The unchanged 30-second LFS expiry and metadata-triggered
enumeration still add background work. To report a change in sustained desktop
CPU utilization, measure both versions over the same fixed observation period
with the real watcher enabled, separate startup from steady state, repeat the
comparison, and record utilization, CPU time, scan counts, and scan-free gaps.

按未舍入的计数器用 `(改前 - 改后) / 改前` 计算，这两轮查询的 Git 进程树累计 CPU
时间下降 40.20%，其中 LFS 进程树下降 52.10%。百分比描述的是这组合成负载的 CPU
时间；Viff 本体与整机 CPU 使用率需要另行测量。现有的 30 秒 LFS 过期重查与元数据
触发枚举仍会产生后台负载。报告桌面持续 CPU 使用率变化时，应在两版启用真实监听器、
使用相同的固定观察时长，将启动阶段与稳定运行阶段分开，重复对照，并记录使用率、
CPU 时间、扫描次数和无扫描间隔。

The November 4, 2026 acceptance used macOS development builds and native Windows
test executables running on Windows Server 2022 Datacenter x64, Git
2.56.0.windows.4, and Microsoft's Basic Display Adapter. The latest result for
each of 298 unique top-level tests passed: domain 25, storage 13, gitcli 83,
app 95, platform 2, UI 80. The Windows run produced 194 final PNGs.

2026 年 11 月 4 日验收使用 macOS 开发构建，以及运行在 Windows Server 2022 Datacenter
x64 上的原生 Windows 测试程序（Git 2.56.0.windows.4、Microsoft Basic Display Adapter）。
298 个唯一顶层测试的最终结果全部通过：domain 25、storage 13、gitcli 83、app 95、
platform 2、UI 80；生成 194 张最终 Windows PNG。

This was not a single all-green first run: 27 initial failures led to fixes for
Windows submodule literal paths, monospace font fallback, and Unix-specific test
assumptions. Full package reruns followed; two UI click synchronization cases
were then verified five times each. Repetitions are not counted as extra tests.
Screenshots in this repository are selected demo images; CI uploads its own
current logs and screenshots as artifacts for 14 days.

首次运行并非全绿：最初的 27 项失败促成 Windows 子模块字面路径、等宽字体回退和 Unix
测试假设的修复。之后完整复跑相关包，再将两个 UI 点击同步用例各验证五轮。重复运行不增加
测试总数。仓库只保留精选演示截图；CI 将当次日志和截图保存为附件，保留 14 天。

## Cross-platform CI and packaging / 跨平台 CI 与安装包

CI runs the Git and Viva headless UI suites on macOS, Windows Server 2022, and
Ubuntu. It also checks native packages: macOS DMG application bundles, Windows
NSIS installers and portable ZIPs, and Linux DEB and portable archives. These checks do not launch
application windows. Review the workflow results for the exact commit before
claiming a release has passed.

CI 在 macOS、Windows Server 2022 和 Ubuntu 运行 Git 与 Viva 无头 UI 测试，
并检查 macOS DMG 应用包、Windows NSIS 安装器与便携 ZIP、Linux DEB 和便携包。
这些检查不打开应用窗口。声明某个版本通过验收前，须检查对应提交的工作流结果。

Ubuntu headless results verify application behavior in the CI renderer. They do
not establish compatibility with every Linux distribution, display server,
graphics driver, input method, or desktop environment. Package validation and
interactive desktop acceptance are separate checks.

Ubuntu 无头结果验证的是 CI 渲染环境下的应用行为，不代表覆盖所有 Linux 发行版、
显示服务器、显卡驱动、输入法或桌面环境。安装包验证与交互式桌面验收分别记录。

## What remains unverified / 尚未验证

The updater has hermetic unit tests (release selection, download, checksum, helper scripts) and headless dialog tests, but the actual file replacement and relaunch on each desktop platform is not exercised in CI.

更新器有隔离的单测（选版、下载、校验、helper 脚本）和无头对话框测试，但各桌面平台的
真实文件替换与重启不在 CI 中执行。

- Windows 10/11 desktop input, IME, and native window interaction / Windows 10/11 桌面输入、输入法及原生窗口交互。
- Signed installers and notarization; real in-app update replacement and restart / 签名安装器与公证；应用内更新的真实替换与重启。
- Public SSH/HTTPS credential prompts and external editor integrations / 公网 SSH/HTTPS 凭据弹窗及外部编辑器集成。
- Linux desktop behavior / Linux 桌面行为。
- Comparative memory or startup benchmarks / 内存和启动速度对比基准。

Windows headless screenshots do not paint every native text-edit overlay;
tests also assert field values and application state. Windows submodule deinit
uses Git's own helper to preserve literal path handling; other Git versions
still need broader compatibility testing.

Windows 无头截图不绘制所有原生文本编辑覆盖层，因此测试也检查字段值和应用状态。
Windows 子模块 deinit 使用 Git 自身 helper 保留字面路径语义；其它 Git 版本仍需更多兼容性测试。
