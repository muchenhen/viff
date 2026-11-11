package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"viff/internal/domain"
)

func TestWindowsWatchLinkedWorktree(t *testing.T) {
	git, main := watchGitFixture(t)
	writeWatchFile(t, filepath.Join(main.Root, "tracked.txt"))
	watchGit(t, git, main.Root, "add", "tracked.txt")
	watchGit(t, git, main.Root, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	linkedPath := filepath.Join(t.TempDir(), "linked")
	watchGit(t, git, main.Root, "worktree", "add", "-qb", "linked", linkedPath)
	linked, err := git.Discover(context.Background(), linkedPath)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := openWindowsWatchRoots(linked)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.close()
	if got := len(roots.reads); got != 2 {
		t.Fatalf("linked worktree has %d subscriptions, want checkout + common dir", got)
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(stop)
	batches := watchBatcher{wake: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan domain.RepoChange, 16)
	listening, delivering := make(chan error, 1), make(chan struct{})
	go func() { listening <- roots.listen(stop, &batches, nil) }()
	go func() {
		defer close(delivering)
		batches.run(ctx, WatchOptions{WorktreeRelevant: func(ctx context.Context, paths []string) (bool, error) {
			return git.WorktreeChangesRelevant(ctx, linked, paths)
		}}, func(change domain.RepoChange) { events <- change })
	}()
	defer func() {
		cancel()
		_ = windows.SetEvent(stop)
		<-delivering
		if err := <-listening; err != nil {
			t.Error(err)
		}
	}()

	// New shared references arrive on the common directory even when the
	// active checkout and its gitfile are in another tree.
	watchGit(t, git, main.Root, "branch", "from-main")
	if change := receiveWatchChange(t, events); !change.Meta || change.Overflow {
		t.Fatalf("shared ref change = %+v", change)
	}
	external := filepath.Join(linked.Root, "external.txt")
	renamed := filepath.Join(linked.Root, "renamed.txt")
	for _, change := range []func() error{
		func() error { return os.WriteFile(external, []byte("new"), 0o644) },
		func() error { return os.WriteFile(external, []byte("edited"), 0o644) },
		func() error { return os.Rename(external, renamed) },
		func() error { return os.Remove(renamed) },
	} {
		if err := change(); err != nil {
			t.Fatal(err)
		}
		if event := receiveWatchChange(t, events); event.Meta || event.Overflow {
			t.Fatalf("worktree change = %+v", event)
		}
	}
	watchGit(t, git, linked.Root, "rm", "tracked.txt")
	if change := receiveWatchChange(t, events); !change.Meta || change.Overflow {
		t.Fatalf("linked index change = %+v", change)
	}
}

func TestWindowsWatchKernelOverflowRearms(t *testing.T) {
	dir, err := resolveWindowsDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(dir.file)
	r, err := newWindowsWatchRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	arm := func() {
		t.Helper()
		if err := r.arm(); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(wantLost bool) {
		t.Helper()
		if wait, err := windows.WaitForSingleObject(r.event, 5000); err != nil || wait != windows.WAIT_OBJECT_0 {
			t.Fatalf("wait = %d, %v", wait, err)
		}
		names := 0
		lost, err := r.complete(func(string) { names++ })
		if err != nil || lost != wantLost || !lost && names == 0 {
			t.Fatalf("completion: lost = %v, names = %d, error = %v", lost, names, err)
		}
	}
	arm()
	writeWatchFile(t, filepath.Join(dir.path, "before.txt"))
	complete(false)
	// The kernel buffers notifications even while no request is pending.
	for i := 0; i < 1600; i++ {
		writeWatchFile(t, filepath.Join(dir.path, fmt.Sprintf("generated-file-with-a-long-name-%05d.txt", i)))
	}
	arm()
	complete(true)
	arm()
	writeWatchFile(t, filepath.Join(dir.path, "after.txt"))
	complete(false)
}

func TestWindowsWatchRecoversReplacedMetadata(t *testing.T) {
	_, repo := watchGitFixture(t)
	events := make(chan domain.RepoChange, 16)
	w, err := Watch(repo, func(change domain.RepoChange) { events <- change }, WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Observe one ordinary notification before replacing the watched dir.
	probe := time.NewTicker(100 * time.Millisecond)
	defer probe.Stop()
	start := time.After(5 * time.Second)
ready:
	for {
		select {
		case <-probe.C:
			writeWatchFile(t, filepath.Join(repo.Root, "probe.txt"))
		case <-events:
			break ready
		case <-start:
			t.Fatal("native watcher did not start")
		}
	}
	if err := os.Rename(repo.MetaDir, filepath.Join(repo.Root, ".git-old")); err != nil {
		t.Fatal(err)
	}
	// Replacing the directory at the same name requires rearming; the
	// recursive checkout handle alone would still see the old path move.
	if err := os.Mkdir(repo.MetaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; {
		if change := receiveWatchChange(t, events); change.Overflow {
			i++ // gap, then successful rearm
		}
	}
	writeWatchFile(t, filepath.Join(repo.MetaDir, "HEAD"))
	if change := receiveWatchChange(t, events); !change.Meta || change.Overflow {
		t.Fatalf("post-rearm metadata event = %+v", change)
	}
}

func TestWindowsWatchAliasesAndResourceLifetime(t *testing.T) {
	_, repo := watchGitFixture(t)
	repo.CommonDir = strings.ToUpper(repo.MetaDir)
	before := processWatchHandles(t)
	goroutines := runtime.NumGoroutine()
	for i := 0; i < 8; i++ {
		roots, err := openWindowsWatchRoots(repo)
		if err != nil {
			t.Fatal(err)
		}
		n := len(roots.reads)
		roots.close()
		if n != 1 {
			t.Fatalf("case alias resulted in %d subscriptions, want one", n)
		}
		w, err := Watch(repo, func(domain.RepoChange) {}, WatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		native, ok := w.(*windowsWatch)
		if !ok {
			w.Close()
			t.Fatal("local NTFS checkout selected polling")
		}
		select {
		case <-native.Ready():
		case <-time.After(5 * time.Second):
			w.Close()
			t.Fatal("watch did not arm")
		}
		if i == 0 {
			t.Logf("live watcher: %d additional handles, %d additional goroutines",
				processWatchHandles(t)-before, runtime.NumGoroutine()-goroutines)
		}
		w.Close()
		w.Close()
	}
	after := processWatchHandles(t)
	t.Logf("process handles %d -> %d; goroutines %d -> %d", before, after, goroutines, runtime.NumGoroutine())
	if after > before+2 || runtime.NumGoroutine() > goroutines {
		t.Fatal("repeated watch/close leaked resources")
	}
}

func TestWindowsWatchDoesNotConfuseCaseSensitiveSiblings(t *testing.T) {
	const root = `\\?\Volume{test}\parent\Repo`
	w := &windowsWatchRoots{repo: domain.Repo{
		Root: root, MetaDir: `\\?\Volume{test}\parent\repo`,
	}}
	batches := watchBatcher{wake: make(chan struct{}, 1)}
	w.notify(root+`\objects\tracked.txt`, &batches)
	batch, _ := batches.takeBefore(time.Time{})
	if _, ok := batch.paths["objects/tracked.txt"]; !ok || batch.change.Meta {
		t.Fatalf("case-sensitive sibling's worktree path was mistaken for Git objects: %+v", batch)
	}
}

func TestWindowsWatchUsesPollingForWSLAndUNC(t *testing.T) {
	// No metadata directories: the fallback can be selected without
	// contacting a live WSL distribution or network share.
	for _, path := range []string{`\\wsl.localhost\distribution\repo`, `\\wsl$\distribution\repo`, `\\server\share\repo`} {
		w, err := Watch(domain.Repo{Root: path}, func(domain.RepoChange) {}, WatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
		if _, ok := w.(*pollWatcher); !ok {
			t.Errorf("%s did not select polling", path)
		}
	}
}

func TestWindowsWatchDetectsResumeFromUnbiasedClock(t *testing.T) {
	now := time.Now().Round(0)
	start := windowsActivity{wall: now, awake: 10_000_000}
	ordinary := windowsActivity{wall: now.Add(time.Second), awake: 20_000_000}
	suspended := windowsActivity{wall: now.Add(time.Minute), awake: 20_000_000}
	if start.resumed(ordinary) || !start.resumed(suspended) {
		t.Fatal("the sleep detector did not distinguish awake time from elapsed time")
	}
}

func processWatchHandles(t *testing.T) uint32 {
	t.Helper()
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	var count uint32
	if ok, _, err := proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count))); ok == 0 {
		t.Fatal(err)
	}
	return count
}

func writeWatchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
}
