package platform

import (
	"context"
	"sync"
	"time"

	"viff/internal/domain"
)

const (
	watchBatchPaths = 256
	// Leave room for literal pathspec prefixes in the Windows command line.
	watchBatchBytes = 16 << 10
)

// watchBatcher decouples kernel reads from a possibly slow Git ignore check.
// There is one in-flight batch and one pending batch. After losing names, the
// pending batch keeps only the request to reconcile against Git.
type watchBatcher struct {
	mu   sync.Mutex
	wake chan struct{}
	next watchBatch
}

type watchBatch struct {
	paths  map[string]struct{}
	bytes  int
	force  bool
	change domain.RepoChange
}

func (b *watchBatcher) add(path string, meta, overflow bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := &b.next
	p.change.Observed = time.Now()
	if path == "" {
		p.force = true
		p.change.Meta = p.change.Meta || meta
		p.change.Overflow = p.change.Overflow || overflow
	} else if !p.force {
		if _, duplicate := p.paths[path]; !duplicate {
			if len(p.paths) >= watchBatchPaths || p.bytes+len(path) > watchBatchBytes {
				p.force, p.change.Meta, p.change.Overflow = true, true, true
			} else {
				if p.paths == nil {
					p.paths = make(map[string]struct{})
				}
				p.paths[path] = struct{}{}
				p.bytes += len(path)
			}
		}
	}
	if p.force {
		p.paths, p.bytes = nil, 0
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *watchBatcher) takeBefore(deadline time.Time) (watchBatch, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.next.change.Overflow {
		// Rechecking while a checkout/extraction is still overflowing
		// typically yields an intermediate snapshot and a second scan.
		const quiet = 750 * time.Millisecond
		due := b.next.change.Observed.Add(quiet)
		if deadline.Before(due) {
			due = deadline
		}
		if delay := time.Until(due); delay > 0 {
			return watchBatch{}, delay
		}
	}
	select {
	case <-b.wake:
	default:
	}
	p := b.next
	b.next = watchBatch{}
	return p, 0
}

func (b *watchBatcher) run(ctx context.Context, opts WatchOptions, changed func(domain.RepoChange)) {
	const debounce = 250 * time.Millisecond
	const minInterval = time.Second
	var next time.Time
	timer := time.NewTimer(0)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
		}
		const maxBurst = 8 * time.Second
		deadline := time.Now().Add(maxBurst)
		delay := max(debounce, time.Until(next))
		var batch watchBatch
		for {
			timer.Reset(delay)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			batch, delay = b.takeBefore(deadline)
			if delay == 0 {
				break
			}
		}
		next = time.Now().Add(minInterval)
		relevant := batch.force || len(batch.paths) != 0
		if !batch.force && relevant && opts.WorktreeRelevant != nil {
			paths := make([]string, 0, len(batch.paths))
			for path := range batch.paths {
				paths = append(paths, path)
			}
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			visible, err := opts.WorktreeRelevant(check, paths)
			cancel()
			relevant = visible || err != nil
		}
		if relevant && ctx.Err() == nil {
			changed(batch.change)
		}
	}
}
