package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"viff/internal/domain"
)

func TestWatchBatchCapsAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := watchBatcher{wake: make(chan struct{}, 1)}
	started := make(chan []string, 1)
	release := make(chan error, 1)
	opts := WatchOptions{WorktreeRelevant: func(ctx context.Context, paths []string) (bool, error) {
		started <- paths
		select {
		case err := <-release:
			return false, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}}
	events := make(chan domain.RepoChange, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.run(ctx, opts, func(change domain.RepoChange) { events <- change })
	}()

	for i := 0; i < 1000; i++ {
		b.add("generated/repeated.txt", false, false)
	}
	select {
	case paths := <-started:
		if len(paths) != 1 {
			t.Fatalf("duplicate paths were not coalesced: %d", len(paths))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("filter did not start")
	}
	for i := 0; i < 10000; i++ {
		b.add(fmt.Sprintf("generated/file-%d.txt", i), false, false)
	}
	release <- nil // first batch was ignored; only the loss is delivered
	if change := receiveWatchChange(t, events); !change.Meta || !change.Overflow || change.Observed.IsZero() {
		t.Fatalf("bounded batch did not request recovery: %+v", change)
	}

	b.add("maybe-visible.txt", false, false)
	select {
	case <-started:
		release <- errors.New("ignore probe unavailable")
	case <-time.After(5 * time.Second):
		t.Fatal("filter did not resume")
	}
	if change := receiveWatchChange(t, events); change.Meta || change.Overflow {
		t.Fatalf("probe failure should request an ordinary refresh: %+v", change)
	}

	b.add("pending.txt", false, false)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("filter did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the watcher left the filter running")
	}
	if len(events) != 0 {
		t.Fatal("canceled filter published a change")
	}
}

func receiveWatchChange(t *testing.T, events <-chan domain.RepoChange) domain.RepoChange {
	t.Helper()
	select {
	case change := <-events:
		return change
	case <-time.After(8 * time.Second):
		t.Fatal("repository change was not delivered")
		return domain.RepoChange{}
	}
}
