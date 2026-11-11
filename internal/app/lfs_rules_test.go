package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"viff/internal/domain"
	"viff/internal/gitcli"
)

type lfsRuleTestBackend struct {
	*refreshBackend
	checks, discoveries, untrackedWalks, statuses atomic.Int32
	discover                                      func(context.Context) error
}

func (b *lfsRuleTestBackend) LFSTrackedAttributes(ctx context.Context, repo domain.Repo, previous *gitcli.LFSIndexCache) (*gitcli.LFSIndexCache, []string, error) {
	b.checks.Add(1)
	if b.discover != nil {
		if err := b.discover(ctx); err != nil {
			return nil, nil, err
		}
	}
	return b.Runner.LFSTrackedAttributes(ctx, repo, previous)
}

func newLFSRuleFixture(t *testing.T) (*fixture, *lfsRuleTestBackend) {
	t.Helper()
	f := newFixture(t)
	base := &refreshBackend{Runner: f.w.Git.(*gitcli.Runner), blocks: make(chan *refreshBlock, 4)}
	b := &lfsRuleTestBackend{refreshBackend: base}
	b.LFSCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte("git-lfs/test"), nil
		}
		b.lists.Add(1)
		return nil, nil
	}
	b.Runner.SetCommandLog(func(_ string, args []string, _ time.Duration, _ error) {
		if slices.Contains(args, ":(glob)**/.gitattributes") && args[0] == "ls-files" {
			b.discoveries.Add(1)
		}
		if slices.Contains(args, "--others") {
			b.untrackedWalks.Add(1)
		}
		if args[0] == "status" {
			b.statuses.Add(1)
		}
	})
	f.w.Git = b
	f.w.Caps = CapabilitiesOf(b)
	return f, b
}

func settleLFSRules(f *fixture) {
	f.t.Helper()
	f.settle(func() bool { return f.idle() && f.w.Repo.lfs.rules.cancel == nil })
}

func assertLFSRules(f *fixture, want ...string) {
	f.t.Helper()
	settleLFSRules(f)
	if st := f.w.Repo.LFS; st.Err != nil || !slices.Equal(st.Patterns, want) {
		f.t.Fatalf("rules = %v (%v), want %v", st.Patterns, st.Err, want)
	}
}

func TestLFSAutomaticRefreshNeverEnumerates(t *testing.T) {
	f, b := newLFSRuleFixture(t)
	f.write(".gitattributes", "*.bin filter=lfs\n")
	start := time.Now()
	f.w.Now = func() time.Time { return start }
	f.open()
	assertLFSRules(f, "*.bin")
	for i := 1; i <= 10; i++ {
		offset := time.Duration(i) * 30 * time.Second
		f.w.Now = func() time.Time { return start.Add(offset) }
		f.w.Refresh(i%2 == 0)
		settleLFSRules(f)
	}
	if b.discoveries.Load() != 1 || b.statuses.Load() != 11 || b.untrackedWalks.Load() != 0 || b.lists.Load() != 0 {
		t.Fatalf("commands after five minutes: discovery=%d status=%d untracked walks=%d LFS listings=%d",
			b.discoveries.Load(), b.statuses.Load(), b.untrackedWalks.Load(), b.lists.Load())
	}
}

func TestLFSRuleCandidatesFollowStatus(t *testing.T) {
	f, b := newLFSRuleFixture(t)
	const dir = "资产 space"
	const attr = dir + "/.gitattributes"
	f.write("base.txt", "base\n")
	f.write(".gitattributes", "*.bin filter=lfs\n")
	f.commitAll("base")
	f.open()
	assertLFSRules(f, "*.bin")

	refresh := func(want ...string) {
		f.w.Refresh(false)
		assertLFSRules(f, want...)
	}
	f.write(attr, "*.psd filter=lfs\n")
	refresh("*.bin", dir+"/*.psd")
	p := filepath.Join(f.dir, attr)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"png", "jpg"} {
		f.write(attr, "*."+ext+" filter=lfs\n")
		if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		refresh("*.bin", dir+"/*."+ext)
	}
	f.write(".gitignore", "/"+dir+"/\n")
	refresh("*.bin")
	f.write(".gitignore", "")
	refresh("*.bin", dir+"/*.jpg")
	if got := b.discoveries.Load(); got != 1 {
		t.Fatalf("untracked and ignore changes caused %d index discoveries, want 1", got)
	}
	f.git("add", "--", attr)
	refresh("*.bin", dir+"/*.jpg")
	f.git("reset", "-q", "HEAD", "--", attr)
	refresh("*.bin", dir+"/*.jpg")
	f.write(".gitignore", "/"+dir+"/\n")
	f.git("add", "-N", "-f", "--", attr)
	f.git("update-index", "--assume-unchanged", "--", attr)
	refresh("*.bin", dir+"/*.jpg")
	f.git("reset", "-q", "HEAD", "--", attr)
	refresh("*.bin")
	f.write(".gitignore", "")
	f.git("add", "--", attr)
	f.git("commit", "-qm", "add nested rules")
	f.git("mv", "--", attr, "renamed-attributes")
	refresh("*.bin")
	if _, kept := f.w.Repo.lfs.rules.parsed[attr]; kept {
		t.Fatal("cache retained a path removed from the candidate set")
	}
	f.git("mv", "--", "renamed-attributes", attr)
	refresh("*.bin", dir+"/*.jpg")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	refresh("*.bin")
	if got := b.untrackedWalks.Load(); got != 0 {
		t.Fatalf("extra untracked discovery calls = %d", got)
	}
}

func TestLFSRulesFailAndRevalidate(t *testing.T) {
	f, b := newLFSRuleFixture(t)
	var fail atomic.Bool
	b.discover = func(ctx context.Context) error {
		if fail.Load() {
			return context.Canceled
		}
		return nil
	}
	var watcher *fakeWatcher
	f.w.Watch = func(_ domain.Repo, changed func(domain.RepoChange)) (Watcher, error) {
		watcher = &fakeWatcher{changed: changed}
		return watcher, nil
	}
	f.write(".gitattributes", "*.bin filter=lfs\n")
	f.open()
	assertLFSRules(f, "*.bin")
	f.w.Now = func() time.Time { return time.Unix(0, 0) }

	f.write(".gitattributes", "*.psd filter=lfs\n")
	fail.Store(true)
	f.w.RefreshManually()
	settleLFSRules(f)
	if st := f.w.Repo.LFS; !errors.Is(st.Err, context.Canceled) || !slices.Equal(st.Patterns, []string{"*.bin"}) {
		t.Fatalf("canceled discovery replaced the successful snapshot: %+v", st)
	}
	f.w.Refresh(false)
	settleLFSRules(f)
	if got := b.checks.Load(); got != 2 {
		t.Fatalf("unchanged status retried during backoff: %d calls", got)
	}
	fail.Store(false)
	watcher.changed(domain.RepoChange{Meta: true, Overflow: true})
	assertLFSRules(f, "*.psd")
	if got := b.discoveries.Load(); got != 2 {
		t.Fatalf("overflow recovery discoveries = %d, want 2", got)
	}

	incomplete := errors.New("incomplete status")
	q := b.block(t, incomplete)
	f.w.Refresh(false)
	waitRefreshStarted(t, q)
	q.unblock()
	settleLFSRules(f)
	if st := f.w.Repo.LFS; !errors.Is(st.Err, incomplete) || !slices.Equal(st.Patterns, []string{"*.psd"}) {
		t.Fatalf("incomplete status replaced the successful snapshot: %+v", st)
	}
	f.write(".gitattributes", strings.Repeat("x", (1<<20)+1))
	f.w.Refresh(false)
	settleLFSRules(f)
	if st := f.w.Repo.LFS; st.Err == nil || !slices.Equal(st.Patterns, []string{"*.psd"}) {
		t.Fatalf("oversized file replaced the successful snapshot: %+v", st)
	}
	f.write(".gitattributes", "*.png filter=lfs\n")
	f.w.RefreshManually()
	assertLFSRules(f, "*.png")
}

func TestLFSRulesAreIsolatedByWorktree(t *testing.T) {
	f, _ := newLFSRuleFixture(t)
	f.write("assets/.gitattributes", "*.one filter=lfs\n")
	f.commitAll("base")
	linked := filepath.Join(t.TempDir(), "linked")
	f.git("worktree", "add", "-qb", "linked", linked)
	if err := os.WriteFile(filepath.Join(linked, "assets", ".gitattributes"), []byte("*.two filter=lfs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.open()
	assertLFSRules(f, "assets/*.one")
	for i := 0; i < 2; i++ {
		old := f.w.Repo
		f.w.Open(linked, func(err error) {
			if err != nil {
				t.Fatal(err)
			}
		})
		f.settle(func() bool { return f.w.Repo != old && f.w.Repo.StatusLoaded })
		assertLFSRules(f, "assets/*.two")
		if old.lfs.rules.index != nil || old.lfs.rules.parsed != nil {
			t.Fatal("switch retained the inactive worktree cache")
		}
		old = f.w.Repo
		f.w.Close()
		if old.lfs.rules.index != nil || old.lfs.rules.parsed != nil {
			t.Fatal("close retained the worktree cache")
		}
		f.open()
		assertLFSRules(f, "assets/*.one")
	}
}

func TestLFSManualRefreshDropsInFlightDiscovery(t *testing.T) {
	f, b := newLFSRuleFixture(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	b.discover = func(ctx context.Context) error {
		if b.checks.Load() != 1 {
			return nil
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	f.write(".gitattributes", "*.bin filter=lfs\n")
	f.open()
	awaitLFS(t, started, "tracked attribute discovery")
	f.write(".gitattributes", "*.psd filter=lfs\n")
	f.w.RefreshManually()
	awaitLFS(t, canceled, "discovery cancellation")
	assertLFSRules(f, "*.psd")
	if got := b.checks.Load(); got != 2 {
		t.Fatalf("discovery count = %d, want canceled pass and one recovery", got)
	}
}
