# 0003 文件监听

- 状态：Windows 部分被 [0049](0049-windows-native-file-watching.md) 替代（2026-11）；其他平台沿用

## 背景
外部编辑器、终端里执行的 Git 命令都会改变仓库，界面需要及时刷新，同时空闲时不能占用 CPU 与内存。

## 决定
- macOS 使用 FSEvents（`internal/platform/watch_darwin.go`）。它按目录树订阅，不需要每个文件一个描述符；同时对 Git 目录做低频指纹校验，以防事件丢失。
- 其他平台暂用有上限的轮询：Git 目录每 5 秒、工作树每 30 秒检查一次。2026-11 从 2 秒/6 秒放宽：每次触发的全量刷新要拉起多个 Git 子进程，Windows 上几百毫秒一轮的持续负载拖慢整机（WSL 仓库例外，见 0038）。原生 watcher 仍是本 ADR 的复审目标。
- 事件合并后再触发一次 `status`；`.git` 内的变化同时刷新引用和日志。

## 代价
Linux 和 Windows 上外部变化最多延迟约 30 秒才刷新（`.git` 内变化仍由 5 秒指纹捕捉），大仓库轮询的持续 I/O 随之降低。

## 复审条件
在 Linux/Windows 上正式支持时，分别接入 inotify 和 ReadDirectoryChangesW。
