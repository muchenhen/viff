# 架构决策记录

按编号记录每个决策的背景、替代项、决定、代价与复审条件。被替代的决策只修改状态，不删除内容。

| 编号 | 决策 |
| --- | --- |
| [0001](0001-git-cli.md) | 通过 Git 命令行访问仓库 |
| [0002](0002-fonts.md) | 界面字体与等宽字体 |
| [0003](0003-file-watching.md) | 文件监听 |
| [0004](0004-icons.md) | 图标来源与编译 |
| [0005](0005-read-only-conflicts.md) | 只读冲突查看器 |
| [0006](0006-deferred-ui-work.md) | 绘制期间产生的延迟工作 |
| [0007](0007-read-only-file-view.md) | 只读文件查看与 blame |
| [0008](0008-compare-commits.md) | 提交比较视图 |
| [0009](0009-patch.md) | 补丁的创建与应用 |
| [0010](0010-submodule-status.md) | 子模块的状态、更新与同步 |
| [0011](0011-log-history-editing.md) | 在 Log 里直接编辑历史 |
| [0012](0012-worktrees.md) | 工作树界面 |
| [0013](0013-smart-checkout.md) | 先 stash 再检出 |
| [0014](0014-push-variants.md) | 推送变体：推到某个提交、别的远端、删除远端标签 |
| [0015](0015-branch-update.md) | 更新未检出的分支与反浅化 |
| [0016](0016-git-settings.md) | Git 命令、签名与凭据设置 |
| [0017](0017-revision-browser.md) | 浏览某个提交的仓库树 |
| [0018](0018-existing-branch-worktree.md) | 从已有分支创建工作树 |
| [0019](0019-git-issue-gaps.md) | Git issue 对照后的功能补齐 |
| [0020](0020-repositories-changelists.md) | 多仓库工作区与 ChangeList |
| [0021](0021-observability.md) | 可观测性、崩溃记录与诊断导出 |
| [0022](0022-child-process-console.md) | 子进程不弹控制台窗口 |
| [0023](0023-app-update.md) | 应用内更新 |
| [0024](0024-file-browser.md) | 只读文件浏览：Project 工具窗口与 Quick Open |
| [0025](0025-svn-support.md) | 增加 Subversion 日常客户端支持 |
| [0026](0026-i18n.md) | 界面语言与消息表 |
| [0027](0027-ai-commit-message.md) | 用模型生成提交信息 |
| [0028](0028-integrated-terminal.md) | 集成终端工具窗 |
| [0029](0029-pr-links.md) | 仓库、提交与分支比较的网页链接 |
| [0030](0030-undo.md) | 会话内全局撤销 |
| [0031](0031-lfs.md) | Git LFS 检测与状态 |
| [0032](0032-ai-conflict-context.md) | 复制冲突上下文给外部 AI |
| [0033](0033-large-repo-performance.md) | 大仓库的 Log 首屏与提交详情 |
| [0034](0034-window-geometry.md) | 记住窗口位置（含副屏） |
| [0035](0035-ai-prompt-rules.md) | 可自定义的提交信息规则 |
| [0036](0036-repository-discovery.md) | 从父目录发现并批量导入仓库 |
| [0037](0037-content-search.md) | 在文件中查找：按需内容搜索 |
| [0038](0038-wsl-repositories.md) | 在 Windows 版直接打开 WSL 仓库 |
| [0039](0039-codemap-in-file-viewer.md) | 代码缩略图只属于只读文件查看器 |
| [0040](0040-branch-double-click.md) | 双击分支默认只筛选，不检出 |
| [0041](0041-commit-time-and-clock-format.md) | 提交时间的时区与时钟格式 |
| [0042](0042-in-app-editing.md) | 工作树文本文件的应用内编辑 |
| [0043](0043-remote-ssh-readonly.md) | 通过 SSH 只读观察远程仓库 |
| [0044](0044-repository-name-search.md) | 欢迎页仓库搜索默认只匹配名称 |
| [0045](0045-diff-folding-and-pane-resize.md) | Diff 折叠未更改片段与可调分栏 |
| [0046](0046-multi-repository-projects.md) | 多仓库项目的 Project 面板聚合（Phase A） |
| [0047](0047-lfs-on-demand-statistics.md) | 按需查询 LFS 文件统计 |
| [0048](0048-lfs-attribute-discovery-cache.md) | 复用 Git 状态并缓存 LFS 属性文件发现 |
| [0049](0049-windows-native-file-watching.md) | Windows 本地 Git 仓库使用原生文件监听 |
