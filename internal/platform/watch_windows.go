package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"viff/internal/domain"
	"viff/internal/wslpath"
)

const (
	windowsPollInterval   = 5 * time.Second
	windowsWatchVerify    = 5 * time.Minute
	windowsWatchRetryMax  = 30 * time.Second
	windowsWatchHeartbeat = time.Second
)

type windowsWatch struct {
	stop   windows.Handle
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan struct{}
	once   sync.Once
}

// Watch uses one native subtree subscription per non-overlapping local Git
// root. UNC, mapped drives, WSL and SVN retain their polling behavior.
func Watch(repo domain.Repo, changed func(domain.RepoChange), opts WatchOptions) (Watcher, error) {
	if wslpath.Is(repo.Root) {
		return startPolling(repo, windowsPollInterval, 1, changed), nil
	}
	if repo.Kind != domain.KindGit || !windowsFixedPath(repo.Root) ||
		repo.MetaDir != "" && !windowsFixedPath(repo.MetaDir) ||
		repo.CommonDir != "" && !windowsFixedPath(repo.CommonDir) {
		return startPolling(repo, windowsPollInterval, 6, changed), nil
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return startPolling(repo, windowsPollInterval, 6, changed), fmt.Errorf("create watcher stop event: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &windowsWatch{stop: stop, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
	batches := watchBatcher{wake: make(chan struct{}, 1)}
	batchDone := make(chan struct{})
	go func() {
		defer close(batchDone)
		batches.run(ctx, opts, changed)
	}()
	go func() {
		defer close(w.done)
		defer func() {
			cancel()
			<-batchDone
		}()
		w.run(ctx, repo, &batches)
	}()
	return w, nil
}

// Ready closes once events are armed or polling has been selected. The first
// status snapshot must start after this point so an edit cannot fall between
// the snapshot and the first subscription.
func (w *windowsWatch) Ready() <-chan struct{} { return w.ready }

func (w *windowsWatch) Close() {
	w.once.Do(func() {
		w.cancel()
		_ = windows.SetEvent(w.stop)
		<-w.done
		windows.CloseHandle(w.stop)
	})
}

func (w *windowsWatch) run(ctx context.Context, repo domain.Repo, batches *watchBatcher) {
	var opened sync.Once
	markReady := func() { opened.Do(func() { close(w.ready) }) }
	defer markReady()
	var fallback *pollWatcher
	defer func() {
		if fallback != nil {
			fallback.Close()
		}
	}()
	retry := time.Second
	armed := false
	for ctx.Err() == nil {
		roots, err := openWindowsWatchRoots(repo)
		if err == nil {
			armed = true
			markReady()
			start := time.Now()
			err = roots.listen(w.stop, batches, func() {
				if fallback != nil {
					fallback.Close()
					fallback = nil
					batches.add("", true, true) // reconcile the gap after rearming
				}
			})
			roots.close()
			// Briefly opening a handle is not recovery if it promptly
			// fails again. Reset the retry delay only after a stable run.
			if time.Since(start) >= windowsWatchRetryMax {
				retry = time.Second
			}
		}
		if ctx.Err() != nil || err == nil {
			return
		}
		if fallback == nil {
			if armed {
				batches.add("", true, true)
			}
			fallback = startPolling(repo, windowsPollInterval, 6, func(change domain.RepoChange) {
				batches.add("", change.Meta, change.Overflow)
			})
			markReady()
		}
		if errors.Is(err, errWindowsWatchUnsupported) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			<-ctx.Done() // stable polling until the repository is reopened
			return
		}
		timer := time.NewTimer(retry)
		select {
		case <-timer.C:
			retry = min(2*retry, windowsWatchRetryMax)
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

type windowsWatchRoots struct {
	dirs  []windowsDir
	reads []*windowsWatchRead
	repo  domain.Repo // normalized physical paths
}

func openWindowsWatchRoots(repo domain.Repo) (*windowsWatchRoots, error) {
	w := &windowsWatchRoots{repo: repo}
	for _, path := range []*string{&w.repo.Root, &w.repo.MetaDir, &w.repo.CommonDir} {
		if *path == "" {
			continue
		}
		var real string
		for _, d := range w.dirs {
			if *path == d.path {
				real = d.real
				break
			}
		}
		if real == "" {
			d, err := resolveWindowsDir(*path)
			if err != nil {
				w.close()
				return nil, err
			}
			w.dirs = append(w.dirs, d)
			real = d.real
		}
		*path = real
	}
	for i, d := range w.dirs {
		covered := false
		for j, other := range w.dirs {
			if i != j {
				// SameFile IDs cover aliases; use the normalized spelling
				// for descendants so case-sensitive siblings are distinct.
				prefix := strings.TrimRight(other.real, `\`) + `\`
				if d.id == other.id && j < i || strings.HasPrefix(d.real, prefix) {
					covered = true
					break
				}
			}
		}
		if covered {
			// Identity is still checked via the original path, but this
			// alias does not need its own directory or completion handle.
			windows.CloseHandle(d.file)
			w.dirs[i].file = 0
			continue
		}
		r, err := newWindowsWatchRead(d)
		if err != nil {
			w.close()
			return nil, err
		}
		w.reads = append(w.reads, r)
		if err := r.arm(); err != nil {
			w.close()
			return nil, err
		}
	}
	return w, nil
}

func windowsRelative(dir, path string) (string, bool) {
	if dir == "" {
		return "", false
	}
	if dir == path {
		return ".", true
	}
	// The directory spelling is normalized by its handle. filepath.Rel
	// folds case on Windows and would conflate NTFS case-sensitive siblings.
	return strings.CutPrefix(path, strings.TrimRight(dir, `\`)+`\`)
}

func (w *windowsWatchRoots) close() {
	for _, r := range w.reads {
		r.close()
	}
	for _, d := range w.dirs {
		if d.file != 0 {
			windows.CloseHandle(d.file)
		}
	}
}

func (w *windowsWatchRoots) listen(stop windows.Handle, batches *watchBatcher, ready func()) error {
	handles := []windows.Handle{stop}
	for _, r := range w.reads {
		handles = append(handles, r.event)
	}
	activity := sampleWindowsActivity()
	nextHeartbeat := time.Now().Add(windowsWatchHeartbeat)
	nextIdentity := time.Now().Add(windowsPollInterval)
	nextVerify := time.Now().Add(windowsWatchVerify)
	for {
		wait, err := windows.WaitForMultipleObjects(handles, false, uint32(windowsWatchHeartbeat.Milliseconds()))
		switch {
		case err != nil:
			return fmt.Errorf("wait for repository notifications: %w", err)
		case wait == windows.WAIT_OBJECT_0:
			return nil
		case wait > windows.WAIT_OBJECT_0 && wait < windows.WAIT_OBJECT_0+uint32(len(handles)):
			r := w.reads[wait-windows.WAIT_OBJECT_0-1]
			lost, err := r.complete(func(path string) {
				w.notify(filepath.Join(r.dir.real, path), batches)
			})
			if err != nil {
				return err
			}
			if lost {
				batches.add("", true, true)
			}
			if err := r.arm(); err != nil {
				return err
			}
		case wait == uint32(windows.WAIT_TIMEOUT):
		default:
			return fmt.Errorf("unexpected repository notification wait result: %d", wait)
		}
		now := time.Now()
		if !now.Before(nextHeartbeat) {
			if ready != nil {
				ready()
				ready = nil
			}
			next := sampleWindowsActivity()
			if activity.resumed(next) {
				batches.add("", true, true)
			} else if next.foreground && !activity.foreground {
				batches.add("", true, false)
			}
			activity, nextHeartbeat = next, now.Add(windowsWatchHeartbeat)
		}
		if !now.Before(nextIdentity) {
			for _, d := range w.dirs {
				if !d.stillPresent() {
					return errors.New("repository watch directory was moved or replaced")
				}
			}
			nextIdentity = now.Add(windowsPollInterval)
		}
		if !now.Before(nextVerify) {
			batches.add("", true, false)
			nextVerify = now.Add(windowsWatchVerify)
		}
	}
}

func (w *windowsWatchRoots) notify(path string, batches *watchBatcher) {
	// MetaDir may be nested under CommonDir. Match the per-worktree dir
	// first so its own lock and log paths are classified relative to it.
	for _, dir := range []string{w.repo.MetaDir, w.repo.CommonDir} {
		if rel, ok := windowsRelative(dir, path); ok {
			if !ignoredGitMetadata(strings.ToLower(filepath.ToSlash(rel))) {
				batches.add("", true, false)
			}
			return
		}
	}
	if rel, ok := windowsRelative(w.repo.Root, path); ok {
		rel = filepath.ToSlash(rel)
		if rel == "." || strings.EqualFold(rel, ".git") {
			// The linked-worktree gitfile or a metadata junction itself.
			batches.add("", true, true)
		} else {
			batches.add(rel, false, false)
		}
	}
}

type windowsActivity struct {
	wall       time.Time
	awake      uint64 // 100 ns units, excluding sleep and hibernation
	foreground bool
}

var queryWindowsAwakeTime = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")

func sampleWindowsActivity() windowsActivity {
	var pid uint32
	_, _ = windows.GetWindowThreadProcessId(windows.GetForegroundWindow(), &pid)
	a := windowsActivity{wall: time.Now().Round(0), foreground: pid == uint32(os.Getpid())}
	if ok, _, _ := queryWindowsAwakeTime.Call(uintptr(unsafe.Pointer(&a.awake))); ok == 0 {
		a.awake = 0
	}
	return a
}

func (a windowsActivity) resumed(next windowsActivity) bool {
	return a.awake != 0 && next.awake >= a.awake &&
		next.wall.Sub(a.wall)-time.Duration(next.awake-a.awake)*100*time.Nanosecond >= 2*time.Second
}
