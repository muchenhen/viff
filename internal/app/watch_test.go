package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"viff/internal/domain"
)

type delayedWatcher struct {
	ready chan struct{}
}

func (w *delayedWatcher) Ready() <-chan struct{} { return w.ready }
func (w *delayedWatcher) Close()                 {}

func TestWatcherArmsBeforeFirstStatusSnapshot(t *testing.T) {
	f := newFixture(t)
	watch := &delayedWatcher{ready: make(chan struct{})}
	w := f.w
	w.Watch = func(_ domain.Repo, _ func(domain.RepoChange)) (Watcher, error) { return watch, nil }
	w.Open(f.dir, nil)
	f.settle(func() bool { return w.Repo != nil })
	if w.Repo.statusSeq != 0 {
		t.Fatal("initial status started before native notifications were armed")
	}
	// An explicit refresh is still allowed during setup. Even if it
	// completes before arming, its snapshot cannot replace the ready pass.
	w.RefreshManually()
	f.settle(func() bool { return w.Repo.StatusLoaded && f.idle() })
	f.write("changed-during-setup.txt", "edit")
	close(watch.ready)
	f.settle(func() bool { return w.Repo.statusSeq == 2 && f.idle() })
	entries := w.Repo.Entries(domain.AreaUntracked)
	if len(entries) != 1 || entries[0].Path != "changed-during-setup.txt" {
		t.Fatalf("initial status did not include edit during setup: %+v", entries)
	}
}

func TestWatcherBatchesAndMergesWithWriteRefresh(t *testing.T) {
	f, _, obs := newRefreshFixture(t)
	w := f.w
	rs := w.Repo
	var notify func(domain.RepoChange)
	w.Watch = func(_ domain.Repo, changed func(domain.RepoChange)) (Watcher, error) {
		notify = changed
		return &fakeWatcher{changed: changed}, nil
	}
	w.startWatcher(rs.Repo)

	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	w.Run("Test write", func(ctx context.Context, _ domain.Repo, _ func(string)) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, RunOptions{})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}

	generation := rs.lfs.rules.generation
	duringWrite := time.Now()
	for i := 0; i < 10000; i++ {
		notify(domain.RepoChange{Meta: i == 5, Overflow: i == 50, Observed: duringWrite})
	}
	if len(w.pending) != 1 {
		t.Fatalf("UI callbacks = %d, want one", len(w.pending))
	}
	w.Drain()
	if rs.refreshing || !rs.watchLost {
		t.Fatal("watch burst did not defer recovery to the write")
	}
	close(release)
	f.settle(f.idle)
	if got := refreshScopes(obs); !reflect.DeepEqual(got, []bool{true}) || rs.lfs.rules.generation == generation {
		t.Fatalf("write and loss should share one validating pass, scopes = %v", got)
	}

	notify(domain.RepoChange{Meta: true, Observed: duringWrite})
	w.Drain()
	if rs.refreshing {
		t.Fatal("delayed write notification repeated the write's refresh")
	}
	notify(domain.RepoChange{Observed: time.Now()})
	f.settle(f.idle)
	if got := refreshScopes(obs); !reflect.DeepEqual(got, []bool{true, false}) {
		t.Fatalf("post-write external change was not refreshed: %v", got)
	}

	w.Close()
	notify(domain.RepoChange{Meta: true, Overflow: true})
	w.Drain()
	if w.Repo != nil || len(refreshScopes(obs)) != 2 {
		t.Fatal("old notification changed the closed repository")
	}
}
