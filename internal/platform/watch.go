// Package platform holds operating-system integration: file-system
// watching and revealing files. OS differences stay in this package.
package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"viff/internal/domain"
)

// Watcher stops a watch.
type Watcher interface{ Close() }

// WatchOptions lets a native watcher ask the Git adapter whether a batch of
// repository-relative, slash-separated worktree paths can change status.
// An absent filter or an error is treated as a reason to recheck.
type WatchOptions struct {
	WorktreeRelevant func(context.Context, []string) (bool, error)
}

// debouncer coalesces bursts of events into one callback: a branch switch
// that touches thousands of files becomes one refresh. meta is sticky within
// a burst so a Git-directory change is never lost behind worktree events.
type debouncer struct {
	mu       sync.Mutex
	timer    *time.Timer
	meta     bool
	overflow bool
	delay    time.Duration
	changed  func(domain.RepoChange)
	closed   bool
}

func newDebouncer(delay time.Duration, changed func(domain.RepoChange)) *debouncer {
	return &debouncer{delay: delay, changed: changed}
}

func (d *debouncer) event(meta, overflow bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.meta = d.meta || meta
	d.overflow = d.overflow || overflow
	if d.timer != nil {
		return
	}
	d.timer = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		m, overflow := d.meta, d.overflow
		d.meta, d.overflow = false, false
		d.timer = nil
		closed := d.closed
		d.mu.Unlock()
		if !closed {
			d.changed(domain.RepoChange{Meta: m, Overflow: overflow})
		}
	})
}

func (d *debouncer) close() {
	d.mu.Lock()
	d.closed = true
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Unlock()
}

// classify decides whether a changed path matters and whether it is Git
// metadata (refs, HEAD, index, operation state) or the working tree.
func classify(repo domain.Repo, path string) (relevant, meta bool) {
	path = filepath.ToSlash(path)
	for _, dir := range []string{repo.MetaDir, repo.CommonDir} {
		if dir == "" {
			continue
		}
		d := filepath.ToSlash(dir)
		if path == d || strings.HasPrefix(path, d+"/") {
			rel := strings.TrimPrefix(strings.TrimPrefix(path, d), "/")
			// Object writes and lock files are followed by the ref or index
			// change that matters; logs only matter to the reflog view.
			if ignoredGitMetadata(rel) {
				return false, false
			}
			return true, true
		}
	}
	return true, false
}

func ignoredGitMetadata(rel string) bool {
	first, rest, _ := strings.Cut(rel, "/")
	if first == "worktrees" {
		_, rest, _ = strings.Cut(rest, "/") // another linked worktree's name
		first, _, _ = strings.Cut(rest, "/")
	}
	return first == "objects" || first == "logs" || first == "lfs" ||
		strings.HasSuffix(rel, ".lock")
}

// pollWatcher is the bounded fallback: it checks a fingerprint of the Git
// directory and lets the caller re-run status at a low rate.
type pollWatcher struct {
	stop chan struct{}
	once sync.Once
}

func (p *pollWatcher) Close() { p.once.Do(func() { close(p.stop) }) }

func gitFingerprint(repo domain.Repo) string {
	var b strings.Builder
	for _, dir := range []string{repo.MetaDir, repo.CommonDir} {
		if dir == "" {
			continue
		}
		for _, name := range []string{"HEAD", "index", "packed-refs", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "FETCH_HEAD"} {
			if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
				b.WriteString(name)
				b.WriteString(st.ModTime().String())
			}
		}
		filepath.WalkDir(filepath.Join(dir, "refs"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, e := d.Info(); e == nil {
					b.WriteString(p)
					b.WriteString(info.ModTime().String())
				}
			}
			return nil
		})
	}
	return b.String()
}

// startPolling checks the Git directory every interval and reports a
// working-tree check every worktreeEvery intervals.
func startPolling(repo domain.Repo, interval time.Duration, worktreeEvery int, changed func(domain.RepoChange)) *pollWatcher {
	p := &pollWatcher{stop: make(chan struct{})}
	go func() {
		last := gitFingerprint(repo)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				n++
				fp := gitFingerprint(repo)
				if fp != last {
					last = fp
					changed(domain.RepoChange{Meta: true})
				} else if worktreeEvery > 0 && n%worktreeEvery == 0 {
					changed(domain.RepoChange{})
				}
			}
		}
	}()
	return p
}
