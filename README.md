<div align="center">

<img src="assets/branding/icon.png" width="128" height="128" alt="Viff" />

# Viff

**A lightweight, native Git and Subversion desktop client, with a project browser and in-app editing of working-tree text files, an English/Chinese interface, and optional AI-written commit messages.**

Go + Viva. No Electron. No WebView. No bundled browser.

[English](README.md) · [简体中文](README.zh-CN.md)

[![CI](https://github.com/1lck/viff/actions/workflows/ci.yml/badge.svg)](https://github.com/1lck/viff/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27.3%2B-00ADD8.svg)](https://go.dev/dl/)

[Features](#a-full-git-workflow) · [Community](#join-the-community) · [Contribute](CONTRIBUTING.md) · [Releases](https://github.com/1lck/viff/releases)

</div>

![Viff in dark mode: checked changes, a side-by-side diff, and commit history](docs/screenshots/diff-dark.png)

## Keep the tool lightweight. Keep the workflow complete.

Git stays the core. Around it, Viff adds a project browser with Quick Open and
in-app editing of working-tree text files, picture diffs, an English/Chinese
interface, and optional model-written commit messages. It is written in Go with
[Viva](https://github.com/vivaui/viva), renders without a browser engine, and
runs your installed Git executable.

- **A small distribution:** one application executable, plus license files. No separate browser runtime or local server to install.
- **A native desktop foundation:** Viva handles rendering and platform integration; Viff does not ship an Electron or WebView application.
- **Git stays Git:** use your repositories, remotes, hooks, and configured credentials. Inspect executed commands and output in the Console.
- **Work is loaded as needed:** paginated history and on-demand diffs keep large repositories from becoming a single eager load.

For a concrete size reference, the stripped executables inside the v0.2.0
packages on November 5, 2026 were **12.13 MiB on macOS arm64** and **13.67 MiB
on Windows amd64** (`-trimpath -ldflags="-s -w"`, executable only). Archive
size, installed Git, and runtime memory are separate measurements.

Lightweight is a design goal, not an unmeasured performance claim. Memory usage
and startup time vary with the repository, platform, and renderer; no comparative
RAM or startup benchmark is claimed.

## Join the community

Welcome to join the Viff community — share your experience, ask questions, and
follow the latest updates.

<p align="center">
  <img src="docs/assets/community/wechat-group.png" width="240" alt="Viff WeChat group QR code">
</p>

<p align="center"><sub>Scan with WeChat to join the group. The group QR code expires
periodically; if it no longer works, open an issue and we will refresh it.</sub></p>

## A full Git workflow

| Work                  | What Viff provides                                                                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Review changes        | Side-by-side diffs with resizable panes and collapsible unchanged stretches, unified diffs, syntax and word highlighting, search, synchronized scrolling, tabs, picture previews, change marks on the right edge   |
| Build a commit        | Check files, folders, or groups; stage files, hunks, or lines; roll back, amend, sign off, and commit/push; fold the editor or write the message with a model |
| Organize work         | Multiple repositories with per-repository commit drafts, a Project panel that groups the workspace and switches the active repository, directory grouping, named ChangeLists, and discovery of repositories below a parent folder |
| Read history          | Commit graph, branch/author/path filters, multi-commit changed-file views, blame, reflog, and revision tree browsing                                          |
| Browse and edit files | Project tool window with folders, status colours, Quick Open, read-only viewing with an optional code map, blame, file history, and editing of working-tree UTF-8 text files (up to 1 MiB, saved text included) |
| Manage branches       | Checkout, merge, rebase, cherry-pick, revert, reset, and stash-assisted checkout                                                                              |
| Undo mistakes         | Command-Z reverses the last local commit, merge, rebase, checkout, or branch change from this session                                                         |
| Resolve conflicts     | Ours / Result / Theirs views, per-block Ours/Theirs/Both actions, explicit mark-as-resolved, and a copyable context for an external AI tool                  |
| Work across checkouts | Create worktrees from new or existing branches; manage submodules with dirty-work protection                                                                  |
| Follow external changes | Native notifications for local NTFS/ReFS Git checkouts on Windows, with five-minute idle verification; [polling elsewhere on Windows](docs/decisions/0049-windows-native-file-watching.md) |
| Track large files     | Detect Git LFS rules and missing git-lfs; count and list managed files on demand from the LFS marker or Git tool window menu                               |
| Exchange changes      | Fetch, pull, push, force-with-lease, remotes, patches, and signing/credential settings                                                                        |
| Work on the web       | Open commits, branches, comparisons, and the pull request form on GitHub, GitLab, Bitbucket, or Gitea                                                        |
| Run commands          | Integrated terminal in the current repository: scrollback, selection copy, paste, clear, restart, and resize                                                   |
| Open WSL repositories | On Windows, open a repository inside a WSL distribution: Git runs in the distribution, files are read through `\\wsl.localhost`                                |
| Observe SSH repositories | Open a working tree on a Linux/macOS server as `ssh://host/path` in read-only mode: status, history, diffs, refs and files come from the server's Git; nothing is written |
| Work in your language | English and Simplified Chinese interface, English by default, switchable in Settings                                                                          |

In Log → Branch, type to search local branches, remote branches, and tags.
Use ↑/↓ and Enter to apply a filter, or Esc to cancel.

**The interface is English by default; switch it to Simplified Chinese in
Settings → General → Language.**

**ChangeLists group files; they do not create separate Git indexes.** A commit
includes **all staged files**, even when a list filter is active. Filtered bulk
staging only stages visible files. Checking a file selects it for bulk actions;
it does not stage it. When checked files are not staged, the Commit panel offers
**Stage Selected** and stages only those files.

**AI commit messages are optional and off by default.** When enabled, the changed
files and their diffs are sent to the endpoint you configure, together with the
commit message rules you wrote or the built-in ones; see the
[privacy guide](docs/privacy.md).

**Copy for AI puts the conflict context on your clipboard.** It is for whatever
external tool you choose; Viff calls no model and sends nothing.

**Content search builds no index.** Find in Files reads the working tree on every
query, so a broad search in a large repository can take seconds; results stop at
300 and ask you to narrow the query.

**ripgrep is optional and not bundled.** The default `auto` setting uses it when
it is installed and falls back to Git grep otherwise.

<details>
<summary>Light theme</summary>

![Viff in light mode with a diff and selected commit](docs/screenshots/diff-light.png)

</details>

Screenshots come from real Viva headless tests using disposable demo repositories.

## Supported version control systems

Viff serves both Git and Subversion repositories from the same window. Entry
points that a backend cannot support are hidden rather than shown and failing;
the reasoning is recorded in [0025](docs/decisions/0025-svn-support.md).

| Capability                                                       | Git                       | Subversion                                 |
| ---------------------------------------------------------------- | ------------------------- | ------------------------------------------ |
| Checkout/open, status, diff, blame, file contents                 | Yes                       | Yes                                        |
| History                                                          | Yes (commit graph)        | Yes (linear, backed by a local cache)      |
| update / commit / revert / add / delete                           | Yes                       | Yes (partial commits via a target set)     |
| Branch switching, creation, deletion                              | Yes                       | Yes (directories plus `switch`)            |
| Conflict display (read-only)                                      | Yes                       | Yes                                        |
| Property editing (`svn:ignore`, `svn:eol-style`)                  | Not applicable            | Not implemented yet                        |
| lock / unlock                                                     | Not applicable            | Not implemented yet                        |
| rebase, cherry-pick, reset, interactive rebase                    | Yes                       | Not applicable                             |
| stash                                                             | Yes                       | Not applicable                             |
| worktree, reflog, submodule                                       | Yes                       | Not applicable                             |
| Staging area (partial staging)                                    | Yes                       | Not applicable                             |

**Subversion differs in three ways that matter day to day:**

- **History needs the repository.** The working copy holds no history, so Viff
  queries the repository and keeps the answer for the session. When the
  repository cannot be reached the failure is reported rather than passed off as
  current data; labelling a kept answer as stale is not implemented yet, so
  nothing is shown as if it were live.
- **No staging area.** Instead of staging, a commit takes an explicit set of
  target files, which you choose in the commit area.
- **Commits are remote and atomic.** Nothing is committed locally, and an
  interrupted commit must be re-checked rather than retried; a finished commit
  cannot be undone locally.

## If macOS says it cannot open `Viff.app`

If macOS says that Apple cannot verify whether `Viff.app` contains malware, the
manually downloaded package may not yet be notarized by Apple. First confirm that
the app came from the trusted [GitHub Releases](https://github.com/1lck/viff/releases),
then use one of these methods:

<p align="center">
  <img src="./docs/assets/screenshots/macos-app-verification-warning.png" width="492" alt="macOS warning that Viff.app cannot be opened">
</p>

1. In **Applications**, Control-click `Viff.app`, choose **Open**, and choose **Open** again in the confirmation dialog.
2. If macOS still blocks it, open **System Settings > Privacy & Security**, click **Open Anyway** next to the security warning, and launch the app again.
3. You can also remove the quarantine attribute in Terminal:

   ```bash
   sudo xattr -dr com.apple.quarantine /Applications/Viff.app
   ```

Only use these steps for an app whose source you trust.

## ⭐ Special thanks

<p align="center">
  <a href="https://linux.do/">
    <img src="./docs/assets/special-thanks/linux-do.png" width="42%" alt="LINUX DO">
  </a>
</p>

<p align="center">
  <strong>For all things AI, head to <a href="https://linux.do/">LINUX DO</a>. Wishing the community ever greater success.</strong>
</p>

## Contributors

Thank you to everyone who contributes to and improves Viff.

<a href="https://github.com/1lck/viff/graphs/contributors">
  <img src="https://raw.githubusercontent.com/1lck/viff/chart-assets/contributors.svg" alt="Contributors">
</a>

## License and credits

Viff's original code is [MIT licensed](LICENSE). Viva, JetBrains Mono, IntelliJ
Platform icons, and other dependencies retain their own licenses; see
[third-party notices](THIRD_PARTY_NOTICES.md).

The interface takes inspiration from IntelliJ IDEA's Git workflow. Viff is an
independent project and is not affiliated with or endorsed by JetBrains.

## Star History

<a href="https://www.star-history.com/#1lck/viff&Date">
  <img alt="Star History Chart" src="https://raw.githubusercontent.com/1lck/viff/chart-assets/star-history-light.svg" width="100%" />
</a>
