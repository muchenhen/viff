<div align="center">

<img src="assets/branding/icon.png" width="128" height="128" alt="Viff" />

# Viff

**轻量、原生的 Git 与 Subversion 桌面客户端，内置项目浏览与工作树文本编辑、中英文界面与可选的 AI 提交说明。**

Go + Viva。无需 Electron，无需 WebView，不捆绑浏览器运行时。

[English](README.md) · [简体中文](README.zh-CN.md)

[![CI](https://github.com/1lck/viff/actions/workflows/ci.yml/badge.svg)](https://github.com/1lck/viff/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27.3%2B-00ADD8.svg)](https://go.dev/dl/)

[功能](#轻量也能覆盖完整的-git-工作流) · [交流群](#加入交流群) · [参与贡献](CONTRIBUTING.md) · [下载](https://github.com/1lck/viff/releases)

</div>

![Viff 深色界面：勾选的变更、并排 Diff 与提交历史](docs/screenshots/diff-dark.png)

## 让工具轻一点，让工作流完整一点

Git 仍是核心。围绕它，Viff 提供项目浏览与 Quick Open、工作树文本编辑、
图片 diff、中英双语界面，以及可选的 AI 提交说明。它使用 Go 和
[Viva](https://github.com/vivaui/viva) 编写，不依赖浏览器引擎绘制界面，
通过本机安装的 Git 执行操作。

- **分发轻量**：一个应用可执行文件，加上许可证文件，不需要安装独立浏览器运行时或本地服务。
- **原生桌面基础**：Viva 负责绘制与平台集成，Viff 不采用 Electron 或 WebView。
- **沿用你的 Git**：使用现有仓库、远端、钩子及凭据配置，在 Console 中查看实际执行的命令和输出。
- **按需加载**：历史分页读取，Diff 按需打开，避免启动时把大仓库的全部内容一次加载进来。

作为可复现的体积参考：2026 年 11 月 5 日发布的 v0.2.0 安装包中，
**macOS arm64 可执行文件为 12.13 MiB，Windows amd64 为 13.67 MiB**
（`-trimpath -ldflags="-s -w"`，仅统计可执行文件）。压缩包大小、本机 Git 和运行内存不在这个数字内。

“轻量”是设计目标，不是未经测量的性能承诺。实际内存与启动耗时取决于仓库、平台和渲染环境，
目前不宣称具体内存占用，也不使用“比某产品快几倍”的对比数字。

## 加入交流群

欢迎加入 Viff 交流群，分享使用体验、提出问题，并获取最新动态。

<p align="center">
  <img src="docs/assets/community/wechat-group.png" width="240" alt="Viff 微信群二维码">
</p>

<p align="center"><sub>使用微信扫码加入交流群。群二维码会周期性过期，如无法加入，欢迎在 Issue 中告知，我们会更新。</sub></p>

## 轻量，也能覆盖完整的 Git 工作流

| 场景       | 功能                                                                                                                              |
| ---------- | --------------------------------------------------------------------------------------------------------------------------------- |
| 查看改动   | 可调整分栏宽度、折叠未更改片段的并排 Diff、统一 Diff、语法与词级高亮、搜索、同步滚动、多页签、图片预览、右边缘改动标记                            |
| 整理提交   | 按文件、目录或整组勾选后暂存，也可按块或选中行暂存；取消暂存、回滚、Amend、Sign-off、提交并推送；说明区可折叠，也可让模型生成说明 |
| 管理工作区 | 多仓库、各仓库独立提交草稿、Project 面板按工作区聚合与切换、目录分组、命名 ChangeList、从父目录发现子仓库                    |
| 阅读历史   | 提交图、分支 / 作者 / 路径筛选、多提交文件汇总、Blame、Reflog、历史仓库树                                                         |
| 浏览与编辑 | Project 面板（目录折叠、状态着色）、Quick Open、只读查看（可选代码缩略图）、Blame、文件历史，以及工作树 UTF-8 文本编辑（不超过 1 MiB，保存后也需在上限内） |
| 操作分支   | Checkout、Merge、Rebase、Cherry-pick、Revert、Reset、借助 Stash 切换分支                                                          |
| 撤销操作   | ⌘Z 撤销本会话内上一次本地提交、合并、rebase、检出或分支改动                                                                       |
| 解决冲突   | Ours / Result / Theirs 三栏，逐块接受 Ours / Theirs / Both，显式标记解决，可复制冲突上下文给外部 AI                               |
| 多工作目录 | 从新分支或已有分支创建 Worktree，管理子模块，保护未提交改动                                                                       |
| 跟踪外部变化 | Windows 本地 NTFS/ReFS Git 仓库使用原生通知，空闲时每五分钟校验一次；Windows 上其他仓库[使用轮询](docs/decisions/0049-windows-native-file-watching.md) |
| 大文件     | 检测 Git LFS 规则与缺失依赖；可从 LFS 标记或 Git 工具窗口菜单按需统计受管文件并复制清单                                            |
| 同步协作   | Fetch、Pull、Push、force-with-lease、远端、补丁、签名与凭据设置                                                                   |
| 网页协作   | 在 GitHub、GitLab、Bitbucket 或 Gitea 打开提交、分支、比较与创建 PR 的页面                                                        |
| 执行命令   | 内置终端，在当前仓库打开；支持滚动回看、选择复制、粘贴、清屏、重启与缩放                                                           |
| 打开 WSL 仓库 | Windows 版可直接打开发行版内的仓库：Git 由发行版执行，文件经 `\\wsl.localhost` 读取                                          |
| 观察远程仓库 | 以 `ssh://host/path` 只读打开 Linux/macOS 服务器上的工作区：状态、历史、diff、引用与文件都来自服务器上的 Git，不写服务器 |
| 使用母语   | 界面支持英文与简体中文，默认英文，可在 设置 → General → Language 切换                                                             |

在 Log → Branch 中可直接输入名称搜索本地分支、远端分支和标签。
用 ↑/↓ 选择、Enter 应用筛选，或按 Esc 取消。

**界面默认英文，可在 设置 → General → Language 切到简体中文。**

**ChangeList 是文件分组，不是独立的 Git 索引。** 即使开启清单筛选，提交仍包含
**全部已暂存文件**；筛选后的批量暂存只处理可见文件。勾选文件只决定批量操作的范围，
不会自动暂存；勾选的未暂存文件可在提交面板用 **Stage Selected** 暂存，且只暂存这些文件。

**AI 生成提交说明是可选的，默认关闭。** 启用后会把变更文件、diff 以及你写的提交信息规则
（没有自定义时为内置规则）发送到你自己配置的端点，边界见[隐私说明](docs/privacy.md)。

**Copy for AI 只把冲突上下文复制到剪贴板**，交给外部工具；Viff 不调用模型、不发送任何内容。

**内容搜索不建索引。** 在文件中查找每次直接读取工作树，大仓库上的宽泛搜索可能需要数秒；
结果最多 300 条，超出时会提示缩小范围。

**ripgrep 是可选项，不随应用打包。** 默认的 `auto` 模式在已安装时优先使用它，否则回退 Git grep。

<details>
<summary>查看浅色主题</summary>

![Viff 浅色界面：Diff 与所选提交详情](docs/screenshots/diff-light.png)

</details>

截图来自真实 Viva 无头测试，使用可销毁的演示仓库。

## 支持的版本控制

Viff 在同一个窗口里同时服务 Git 与 Subversion 仓库。后端不支持的能力入口会被隐藏，
而不是显示出来再报错；取舍理由见 [0025](docs/decisions/0025-svn-support.md)。

| 能力                                              | Git                  | Subversion                     |
| ------------------------------------------------- | -------------------- | ------------------------------ |
| 检出/打开、状态、diff、blame、文件内容            | 有                   | 有                             |
| 历史                                              | 有（提交图）         | 有（线性，需要本地缓存）       |
| update / commit / revert / add / delete           | 有                   | 有（部分提交用目标集表达）     |
| 分支切换、创建、删除                              | 有                   | 有（目录 + `switch`）          |
| 冲突只读展示                                      | 有                   | 有                             |
| 属性编辑（`svn:ignore`、`svn:eol-style`）         | 不适用               | 尚未实现                       |
| lock / unlock                                     | 不适用               | 尚未实现                       |
| rebase、cherry-pick、reset、交互式 rebase         | 有                   | 不适用                         |
| stash                                             | 有                   | 不适用                         |
| worktree、reflog、submodule                       | 有                   | 不适用                         |
| 暂存区（部分暂存）                                | 有                   | 不适用                         |

**Subversion 有三处日常使用中会遇到的差异：**

- **历史需要访问仓库。** 工作副本本身不含历史，所以 Viff 向仓库查询并在本次会话内保留结果；
  仓库不可达时直接报错，不把旧结果当成最新数据。把保留的结果显式标注为陈旧还未实现，
  因此界面上不会出现「看起来是最新、实际是缓存」的内容。
- **没有暂存区。** 提交改用显式的提交目标集：在提交区勾选本次要提交的文件。
- **提交是远程原子操作。** 没有本地提交；中断后必须重新查询工作副本而不是自动重试，
  已完成的提交也无法在本地撤销。

## macOS 提示“无法打开 Viff.app”

如果 macOS 提示“Apple 无法验证 Viff.app 是否包含可能危害 Mac 安全或泄漏隐私的恶意软件”，
通常是因为手动下载的安装包尚未经过 Apple 公证。请先确认应用来自可信的
[GitHub Releases](https://github.com/1lck/viff/releases)，然后选择以下任一方式：

<p align="center">
  <img src="./docs/assets/screenshots/macos-app-verification-warning.png" width="492" alt="macOS 提示无法打开 Viff.app">
</p>

1. 在“应用程序”中按住 Control 键点按 `Viff.app`，选择“打开”，再在确认对话框中选择“打开”。
2. 如果仍被阻止，打开“系统设置 > 隐私与安全性”，在安全性提示旁点按“仍要打开”，然后再次启动应用。
3. 也可以在终端中移除下载文件的隔离标记：

   ```bash
   sudo xattr -dr com.apple.quarantine /Applications/Viff.app
   ```

上述命令只应对你确认来源可靠的应用使用。

## ⭐ 特别鸣谢

<p align="center">
  <a href="https://linux.do/">
    <img src="./docs/assets/special-thanks/linux-do.png" width="42%" alt="LINUX DO">
  </a>
</p>

<p align="center">
  <strong>关于 AI 的一切，欢迎前往 <a href="https://linux.do/">LINUX DO</a>！祝社区越来越好～</strong>
</p>

## 贡献者

感谢所有参与 Viff 开发和改进的贡献者。

<a href="https://github.com/1lck/viff/graphs/contributors">
  <img src="https://raw.githubusercontent.com/1lck/viff/chart-assets/contributors.svg" alt="贡献者">
</a>

## 许可与致谢

Viff 原创代码采用 [MIT 许可证](LICENSE)。Viva、JetBrains Mono、IntelliJ Platform 图标及其它依赖
保留各自许可，详见[第三方声明](THIRD_PARTY_NOTICES.md)。

界面参考了 IntelliJ IDEA 的 Git 工作流。Viff 是独立项目，与 JetBrains 没有关联，也未获得其背书。

## Star History

<a href="https://www.star-history.com/#1lck/viff&Date">
  <img alt="Star History 图表" src="https://raw.githubusercontent.com/1lck/viff/chart-assets/star-history-light.svg" width="100%" />
</a>
