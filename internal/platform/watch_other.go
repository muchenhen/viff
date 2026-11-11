//go:build !darwin && !windows

package platform

import (
	"time"

	"viff/internal/domain"
	"viff/internal/wslpath"
)

// Watch falls back to a bounded check on platforms without a native watcher
// wired up yet: the Git directory every 5 seconds and the working tree every
// 30 seconds. The cadence is a compromise: every triggered refresh spawns
// several Git processes, which on Windows costs hundreds of milliseconds and
// constant background load if it runs every few seconds.
//
// A WSL repository is watched more slowly: every poll round-trips the 9P file
// system behind \\wsl.localhost, where a stat costs orders of magnitude more
// than on a local disk. Five seconds keeps changes visible while leaving the
// redirector room; tuning belongs to a measurement, not a guess (ADR 0038).
func Watch(repo domain.Repo, changed func(domain.RepoChange), _ WatchOptions) (Watcher, error) {
	interval, worktreeEvery := 5*time.Second, 6
	if wslpath.Is(repo.Root) {
		// Every poll here already round-trips 9P, so the working tree is not
		// sampled less often than the Git directory: the documented five
		// seconds is what both get.
		interval, worktreeEvery = 5*time.Second, 1
	}
	return startPolling(repo, interval, worktreeEvery, changed), nil
}
