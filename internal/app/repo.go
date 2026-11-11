package app

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"viff/internal/domain"
	"viff/internal/gitcli"
	"viff/internal/i18n"
	"viff/internal/observability"
	"viff/internal/sshpath"
)

// RepoState is everything known about the open repository. Each field is
// replaced as a whole when a query returns.
type RepoState struct {
	Repo domain.Repo
	// Gen increases whenever the repository is reopened; results for an older
	// generation are dropped. Workspace.nextGen issues it and never reuses a
	// value, so closing and reopening also yields a new generation.
	Gen uint64
	// rootKey is the canonical root used as the workspace list key.
	rootKey string

	Status       domain.Status
	StatusLoaded bool
	StatusTime   time.Time
	Operation    domain.Operation
	Conflicts    []domain.Conflict
	Refs         []domain.Ref
	Remotes      []domain.Remote
	Stashes      []domain.Stash
	RefsLoaded   bool
	// RemoteHeads maps a remote to the branch its HEAD points at, the default
	// branch a pull request targets. Git writes refs/remotes/<remote>/HEAD; a
	// remote without it is absent, and BaseBranch falls back to main/master.
	RemoteHeads map[string]string
	// RefsByHash maps a commit to the refs pointing at it, for the log.
	RefsByHash map[string][]domain.Ref

	// Worktrees are the linked checkouts of this repository. A branch can be
	// checked out in only one worktree, so the branch popup needs the list to
	// say where a branch lives and where to open it.
	Worktrees       []domain.Worktree
	WorktreesLoaded bool

	// Shallow marks a clone whose history is truncated. The Git window offers
	// to fetch the missing history while it is set.
	Shallow bool

	// LFS is background rule detection; LFSStats is an explicit file listing.
	LFS      gitcli.LFSStatus
	LFSStats LFSStats
	lfs      lfsState

	Log LogState

	// Detail is the selected commit's detail in the log.
	Detail         *domain.CommitDetail
	DetailHash     string
	DetailLoading  bool
	DetailBranches []string
	// detailSeq stamps each detail load so a late result for the same commit
	// cannot replace a newer one; detailCancel stops its queries.
	detailSeq    uint64
	detailCancel context.CancelFunc

	statusSeq     uint64 // increments for each status query
	refreshing    bool
	refreshMore   bool
	refreshMeta   bool // OR of metadata requests in the pending refresh only
	refreshCancel context.CancelFunc
	// Write completion covers watcher observations already made. An event
	// observed after this point still needs a new pass.
	watchCovered time.Time
	watchWrites  int
	watchLost    bool
	refsSeq      uint64
	refsKey      string
}

// writeLocks serializes writes per common Git directory: worktrees of one
// repository share refs and objects.
var writeLocks sync.Map

func writeLock(repo domain.Repo) *sync.Mutex {
	m, _ := writeLocks.LoadOrStore(repo.LockKey(), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Open opens the repository containing dir. done reports failure on the UI
// thread. Opening a repository while another one is active is a switch, so a
// running write task refuses it just like ActivateRepository does.
//
// Open shares the activation sequence with ActivateRepository and Close: a slow
// discovery that finishes after a newer open, switch or close is stale and
// must not replace that newer state.
func (w *Workspace) Open(dir string, done func(error)) {
	if w.Repo != nil && w.Busy() {
		if done != nil {
			done(ErrRepositoryBusy)
		}
		return
	}
	w.repos.activateSeq++
	seq := w.repos.activateSeq
	start := time.Now()
	_, ctx, finish := w.startTask("Opening "+displayBase(dir), false)
	go func() {
		backend := w.pickBackend(dir)
		if backend == nil {
			w.post(func() {
				finish()
				if done != nil {
					done(ErrUnsupported)
				}
			})
			return
		}
		repo, err := backend.Discover(ctx, dir)
		took := time.Since(start)
		w.post(func() {
			finish()
			if seq != w.repos.activateSeq {
				// A newer open, switch or close replaced this request.
				w.record(observability.Event{
					Event: observability.EventRepoOpen, Level: observability.LevelWarn,
					Repo: dir, Op: "open", Dur: took,
					Fields: map[string]any{"status": "stale"},
				})
				if done != nil {
					done(ErrRepositoryStale)
				}
				return
			}
			if err != nil {
				w.record(observability.Event{
					Event: observability.EventRepoOpen, Level: observability.LevelError,
					Repo: dir, Op: "open", Code: gitcli.CodeOf(err), Dur: took,
					Fields: map[string]any{"status": "failed"},
				})
				if done != nil {
					done(err)
				}
				return
			}
			// A write task may have started while discovery ran; opening a
			// repository is a switch from the existing one, so refuse it the
			// same way ActivateRepository does.
			if w.Busy() {
				w.record(observability.Event{
					Event: observability.EventRepoOpen, Level: observability.LevelWarn,
					Repo: dir, Op: "open", Dur: took,
					Fields: map[string]any{"status": "busy"},
				})
				if done != nil {
					done(ErrRepositoryBusy)
				}
				return
			}
			w.useBackend(backend)
			w.setRepo(repo)
			w.record(observability.Event{
				Event: observability.EventRepoOpen, Level: observability.LevelInfo,
				Repo: repo.Root, Op: "open", Dur: took,
				Fields: map[string]any{"status": "ok", "worktree": repo.IsLinkedWorktree()},
			})
			if done != nil {
				done(nil)
			}
		})
	}()
}

func (w *Workspace) setRepo(repo domain.Repo) {
	// The repository becomes part of the explicit workspace list and the
	// active one; this also persists the list before any command runs.
	root := w.noteActiveRepository(repo)
	oldRoot := ""
	if w.Repo != nil {
		oldRoot = w.Repo.Repo.Root
		w.Repo.Log.close()
		w.Repo.cancelDetail()
		w.Repo.cancelRefresh()
		w.cancelLFS(w.Repo)
	}
	w.closeWatcher(oldRoot)
	// A content search belongs to the repository that started it.
	w.cancelSearch()
	// The undo history describes one checkout's HEAD and branches; a switch
	// makes every record describe a state that is no longer current.
	w.clearUndo()
	w.nextGen++
	w.Repo = &RepoState{Repo: repo, Gen: w.nextGen, rootKey: root}
	if scan := w.lfsScans[root]; scan != nil && scan.unconfirmed {
		w.Repo.LFSStats.Status, w.Repo.LFSStats.Code = LFSStatsFailed, gitcli.CodeOf(gitcli.ErrLFSReapUnconfirmed)
	}
	if w.Settings != nil {
		w.Settings.AddRecent(repo.Root)
		w.Settings.Save()
	}
	w.startWatcher(repo)
	if watch, ok := w.watcher.(interface{ Ready() <-chan struct{} }); ok {
		state, ready := w.Repo, watch.Ready()
		go func() {
			<-ready
			w.post(func() {
				if w.Repo == state {
					w.Refresh(true)
				}
			})
		}()
	} else {
		w.Refresh(true)
	}
	w.ReloadLog()
}

// startWatcher starts the file-system watch of repo. The backend is chosen by
// the Watch function the composition root installs, so the event leaves
// backend empty rather than guessing between native and polling.
func (w *Workspace) startWatcher(repo domain.Repo) {
	if repo.IsSSH() {
		w.watcher = w.startRemoteWatch(repo)
		return
	}
	if w.Watch == nil {
		return
	}
	state := w.Repo
	// A minimized or busy window need not drain once per OS delivery.
	// Keep one UI notification, retaining the strongest scope.
	var mu sync.Mutex
	var pending domain.RepoChange
	posted := false
	watcher, err := w.Watch(repo, func(change domain.RepoChange) {
		mu.Lock()
		pending.Meta = pending.Meta || change.Meta
		pending.Overflow = pending.Overflow || change.Overflow
		if !posted || change.Observed.IsZero() || !pending.Observed.IsZero() && change.Observed.After(pending.Observed) {
			pending.Observed = change.Observed
		}
		if posted {
			mu.Unlock()
			return
		}
		posted = true
		mu.Unlock()
		w.post(func() {
			mu.Lock()
			change := pending
			pending, posted = domain.RepoChange{}, false
			mu.Unlock()
			w.applyRepoChange(state, change)
		})
	})
	if err != nil {
		w.record(observability.Event{
			Event: observability.EventRepoWatch, Level: observability.LevelWarn,
			Repo: repo.Root, Op: "watch",
			Fields: map[string]any{"status": "error", "backend": "", "error": truncateBytes(err.Error(), 512)},
		})
		if watcher == nil {
			return
		}
	}
	w.watcher = watcher
	w.record(observability.Event{
		Event: observability.EventRepoWatch, Level: observability.LevelInfo,
		Repo: repo.Root, Op: "watch",
		Fields: map[string]any{"status": "started", "backend": ""},
	})
}

func (w *Workspace) applyRepoChange(state *RepoState, change domain.RepoChange) {
	if w.Repo != state {
		return
	}
	if state.watchWrites != 0 {
		state.watchLost = state.watchLost || change.Overflow
		return
	}
	if change.Overflow {
		w.RefreshManually()
	} else if change.Observed.IsZero() || change.Observed.After(state.watchCovered) {
		w.Refresh(change.Meta)
	}
}

// closeWatcher stops the current watcher, if any, and records it. root is the
// repository the watcher watched, "" when unknown.
func (w *Workspace) closeWatcher(root string) {
	if w.watcher == nil {
		return
	}
	w.watcher.Close()
	w.watcher = nil
	w.record(observability.Event{
		Event: observability.EventRepoWatch, Level: observability.LevelInfo,
		Repo: root, Op: "watch",
		Fields: map[string]any{"status": "stopped", "backend": ""},
	})
}

// Close closes the repository and returns to the welcome screen. It releases
// the repository-specific log and file watcher; the persisted workspace list
// and the persisted active repository are left alone, so closing (or quitting)
// never forgets the repositories the user added. RemoveRepository is the only
// operation that clears the active root, and it does so explicitly.
func (w *Workspace) Close() {
	// Invalidate opens and switches that are still discovering: a repository
	// that is being opened must not reopen the window after it was closed.
	w.repos.activateSeq++
	root := ""
	if w.Repo != nil {
		root = w.Repo.Repo.Root
		w.record(observability.Event{
			Event: observability.EventRepoClose, Level: observability.LevelInfo,
			Repo: root, Op: "close",
			Fields: map[string]any{
				"op_state":      operationStateName(w.Repo.Operation),
				"tasks_running": len(w.Tasks),
				"watcher":       w.watcher != nil,
			},
		})
		w.Repo.Log.close()
		w.Repo.cancelDetail()
		w.Repo.cancelRefresh()
		w.cancelLFS(w.Repo)
	}
	w.closeWatcher(root)
	w.clearUndo()
	w.cancelSearch()
	w.Repo = nil
}

// operationStateName is the stable name of the in-progress operation used in
// event fields, "none" when the working tree is not in the middle of one.
func operationStateName(op domain.Operation) string {
	switch op.Kind {
	case domain.OpMerge:
		return "merge"
	case domain.OpRebase:
		return "rebase"
	case domain.OpCherryPick:
		return "cherry-pick"
	case domain.OpRevert:
		return "revert"
	case domain.OpApplyMailbox:
		return "apply-mailbox"
	case domain.OpBisect:
		return "bisect"
	}
	return "none"
}

// refreshIfIdle starts a refresh only when none is running. It reports whether
// it started one; the remote watcher uses that answer to decide whether its
// status key is now accounted for. It runs on the UI thread.
func (w *Workspace) refreshIfIdle(meta bool) bool {
	rs := w.Repo
	if rs == nil || rs.refreshing {
		return false
	}
	w.Refresh(meta)
	return true
}

// RefreshManually revalidates attribute discovery even if the status keys
// match. Watchers also use this recovery path when events were dropped.
func (w *Workspace) RefreshManually() {
	if rs := w.Repo; rs != nil {
		invalidateLFSRules(rs)
		w.Refresh(true)
	}
}

func (rs *RepoState) cancelRefresh() {
	if rs.refreshCancel != nil {
		rs.refreshCancel()
		rs.refreshCancel = nil
	}
}

// Refresh queries status and operation state again, and refs when meta is
// set. Overlapping requests share one pending refresh, whose metadata scope is
// the OR of those requests, independent of the running refresh. Each pass
// consumes that pending request before starting the next bounded merge.
func (w *Workspace) Refresh(meta bool) {
	rs := w.Repo
	if rs == nil {
		return
	}
	if rs.refreshing {
		rs.refreshMore = true
		rs.refreshMeta = rs.refreshMeta || meta
		return
	}
	rs.refreshing = true
	start := time.Now()
	rs.statusSeq++
	seq := rs.statusSeq
	repo := rs.Repo
	generation := rs.lfs.rules.generation
	git, caps := w.Git, w.Caps
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	rs.refreshCancel = cancel
	go func() {
		defer cancel()
		// The queries are reads against the immutable repo identity; the
		// groups are independent, so they run concurrently instead of paying
		// one subprocess spawn after another (which dominates first paint on
		// Windows). Overlapping refreshes stay coalesced by rs.refreshing.
		var (
			st           domain.Status
			err          error
			op           domain.Operation
			conflicts    []domain.Conflict
			refs         []domain.Ref
			remotes      []domain.Remote
			stashes      []domain.Stash
			refsErr      error
			worktrees    []domain.Worktree
			worktreesErr error
			shallow      bool
			shallowErr   error
			remoteHeads  map[string]string
			headsErr     error
		)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			st, err = git.Status(ctx, repo)
			if err == nil {
				op = git.Operation(ctx, repo)
				conflicts, _ = git.Conflicts(ctx, repo, st)
			}
		}()
		go func() {
			defer wg.Done()
			refs, refsErr = git.Refs(ctx, repo)
			if refsErr == nil {
				remotes, _ = git.Remotes(ctx, repo)
				if caps.Stash != nil {
					stashes, _ = caps.Stash.Stashes(ctx, repo)
				}
			}
		}()
		go func() {
			defer wg.Done()
			if caps.Worktrees != nil {
				worktrees, worktreesErr = caps.Worktrees.Worktrees(ctx, repo)
			}
			shallow, shallowErr = git.IsShallow(ctx, repo)
			// The remote HEAD names the branch a pull request targets; it is
			// read with the other refs so a comparison never guesses while it
			// is known.
			if h, ok := git.(remoteHeadReader); ok {
				remoteHeads, headsErr = h.RemoteHeads(ctx, repo)
			}
		}()
		wg.Wait()
		w.post(func() {
			if w.Repo != rs || rs.statusSeq != seq {
				return
			}
			rs.refreshing = false
			rs.refreshCancel = nil
			staged, unstaged, untracked, conflicted := 0, 0, 0, 0
			if err != nil {
				w.markRepositoryError(rs.rootKey, err)
				if gitcli.KindOf(err) == gitcli.ErrNotRepository {
					w.Notify(NoticeError, w.tr(i18n.AppNoticeRepositoryGone), repo.Root)
					// Keep the row (with its error) but there is nothing to be
					// active any more.
					w.Close()
					if w.ActiveRoot == rs.rootKey {
						w.ActiveRoot = ""
						w.saveRepositories()
					}
					return
				}
				w.NotifyError(w.tr(i18n.AppNoticeCannotReadStatus), err)
			} else {
				if rs.StatusLoaded && lfsStatusInputsChanged(rs.Status, st) {
					w.invalidateLFSStats(rs)
				}
				rs.Status = st
				rs.StatusLoaded = true
				rs.StatusTime = w.Now()
				rs.Operation = op
				rs.Conflicts = conflicts
				staged, unstaged, untracked, conflicted = st.Counts()
				// Git reports a rename as one entry whose old path is gone;
				// keep change-list membership attached to the new path. A
				// failure is reported once per failure streak, not on every
				// refresh.
				w.warnChangeListSync(w.SyncChangeListRenames())
			}
			if refsErr == nil {
				key := refsFingerprint(refs, st.Branch)
				changed := key != rs.refsKey
				rs.Refs, rs.Remotes, rs.Stashes, rs.RefsLoaded = refs, remotes, stashes, true
				rs.RefsByHash = indexRefs(refs, st.Branch)
				if changed {
					first := rs.refsKey == ""
					rs.refsKey = key
					if !first || rs.Log.Loaded() {
						w.ReloadLog()
					}
				}
			}
			if worktreesErr == nil {
				rs.Worktrees, rs.WorktreesLoaded = worktrees, true
			}
			if shallowErr == nil {
				rs.Shallow = shallow
			}
			if headsErr == nil {
				rs.RemoteHeads = remoteHeads
			}
			// Keep the workspace overview row of the active repository in
			// step with the view; a failure keeps the known status.
			w.syncActiveSummary(err)
			if err == nil {
				w.record(observability.Event{
					Event: observability.EventRepoRefresh, Level: observability.LevelInfo,
					Repo: repo.Root, Op: "refresh", Dur: time.Since(start),
					Fields: map[string]any{
						"gen":        rs.Gen,
						"meta":       meta,
						"staged":     staged,
						"unstaged":   unstaged,
						"untracked":  untracked,
						"conflicted": conflicted,
						"op_state":   operationStateName(op),
						"lfs":        lfsEventFields(rs.LFS, rs.LFSStats),
					},
				})
			}
			if generation == rs.lfs.rules.generation {
				if err == nil {
					w.refreshLFS(rs, st)
				} else if _, ok := w.Git.(lfsReporter); ok {
					invalidateLFSRules(rs)
					rs.LFS.Err = err
				}
			}
			if rs.refreshMore {
				meta := rs.refreshMeta
				rs.refreshMore, rs.refreshMeta = false, false
				if meta {
					// Deepening history need not move any ref. Invalidate with
					// the pending metadata pass, after the old pass publishes.
					rs.refsKey = ""
				}
				w.Refresh(meta)
			}
		})
	}()
}

func refsFingerprint(refs []domain.Ref, branch domain.BranchStatus) string {
	var b strings.Builder
	b.WriteString(branch.Head)
	b.WriteByte(0)
	b.WriteString(branch.Upstream)
	b.WriteByte(0)
	for _, r := range refs {
		b.WriteString(r.Name)
		b.WriteByte(0)
		b.WriteString(r.Target)
		b.WriteByte(0)
	}
	return b.String()
}

func indexRefs(refs []domain.Ref, branch domain.BranchStatus) map[string][]domain.Ref {
	m := make(map[string][]domain.Ref, len(refs))
	if branch.Head != "" {
		m[branch.Head] = append(m[branch.Head], domain.Ref{Kind: domain.RefHead, Short: "HEAD", Name: "HEAD", Target: branch.Head})
	}
	for _, r := range refs {
		m[r.Target] = append(m[r.Target], r)
	}
	for h, list := range m {
		sort.SliceStable(list, func(i, j int) bool { return refOrder(list[i]) < refOrder(list[j]) })
		m[h] = list
	}
	return m
}

func refOrder(r domain.Ref) int {
	switch {
	case r.Kind == domain.RefHead:
		return 0
	case r.Kind == domain.RefLocal && r.Current:
		return 1
	case r.Kind == domain.RefLocal:
		return 2
	case r.Kind == domain.RefRemote:
		return 3
	}
	return 4
}

// CurrentBranch is the checked-out branch name, "" when detached.
func (rs *RepoState) CurrentBranch() string { return rs.Status.Branch.Branch }

// Refreshing reports whether a status query is still in flight. The window
// reads it on the UI thread like the rest of the state; the headless fixture
// waits on it so a test reads the state a write produced, not the one before.
func (rs *RepoState) Refreshing() bool { return rs.refreshing }

// CurrentRef returns the local ref of the current branch.
func (rs *RepoState) CurrentRef() (domain.Ref, bool) {
	for _, r := range rs.Refs {
		if r.Kind == domain.RefLocal && r.Current {
			return r, true
		}
	}
	return domain.Ref{}, false
}

// WorktreeOf returns the worktree that has branch checked out.
func (rs *RepoState) WorktreeOf(branch string) (domain.Worktree, bool) {
	if branch == "" {
		return domain.Worktree{}, false
	}
	for _, wt := range rs.Worktrees {
		// A prunable worktree's directory is gone, so it cannot be opened and
		// no longer holds the branch in practice.
		if wt.Prunable || wt.Bare {
			continue
		}
		if wt.Branch == branch {
			return wt, true
		}
	}
	return domain.Worktree{}, false
}

// WorktreeElsewhere returns the other worktree that has branch checked out,
// that is, not the checkout this state belongs to. A branch can be checked
// out once, so the popup opens that worktree instead of failing a checkout.
func (rs *RepoState) WorktreeElsewhere(branch string) (domain.Worktree, bool) {
	wt, ok := rs.WorktreeOf(branch)
	if !ok || samePath(wt.Path, rs.Repo.Root) {
		return domain.Worktree{}, false
	}
	return wt, true
}

// IsCurrentWorktree reports whether wt is the checkout this state belongs to.
func (rs *RepoState) IsCurrentWorktree(wt domain.Worktree) bool {
	return samePath(wt.Path, rs.Repo.Root)
}

// samePath reports whether two paths name one directory, following symlinks
// (on macOS /tmp is a link to /private/tmp).
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if sshpath.Is(a) || sshpath.Is(b) {
		na, oka := sshpath.Normalize(a)
		nb, okb := sshpath.Normalize(b)
		return oka && okb && na == nb
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// HeadLabel is the branch widget text: the branch, or the short hash when
// detached, with the operation in progress.
func (rs *RepoState) HeadLabel() string {
	b := rs.Status.Branch
	label := b.Branch
	if b.Detached || label == "" {
		label = domain.ShortHash(b.Head)
		if label == "" {
			label = "No commits"
		}
	}
	if rs.Operation.Kind == domain.OpRebase && rs.Operation.HeadName != "" {
		label = "Rebasing " + rs.Operation.HeadName
	}
	return label
}

// HasHead reports a repository with at least one commit.
func (rs *RepoState) HasHead() bool { return rs.Status.Branch.Head != "" }

// Locals, RemoteRefs and Tags filter refs by kind.
func (rs *RepoState) RefsOf(kind domain.RefKind) []domain.Ref {
	var out []domain.Ref
	for _, r := range rs.Refs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// Entries returns the status entries shown in an area.
func (rs *RepoState) Entries(area domain.Area) []domain.StatusEntry {
	var out []domain.StatusEntry
	for _, e := range rs.Status.Entries {
		switch area {
		case domain.AreaStaged:
			if e.Staged() {
				out = append(out, e)
			}
		case domain.AreaUnstaged:
			if e.Unstaged() {
				out = append(out, e)
			}
		case domain.AreaUntracked:
			if e.Untracked() {
				out = append(out, e)
			}
		case domain.AreaConflicts:
			if e.Conflicted() {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Conflict returns the conflict record for path.
func (rs *RepoState) Conflict(path string) (domain.Conflict, bool) {
	for _, c := range rs.Conflicts {
		if c.Path == path {
			return c, true
		}
	}
	return domain.Conflict{}, false
}

// exists reports whether a path exists in the working tree.
func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// displayBase is the short name of a repository root for a task title: the
// folder name locally, the last remote path segment for an SSH repository.
func displayBase(root string) string {
	if sshpath.Is(root) {
		return sshpath.Name(root)
	}
	return filepath.Base(root)
}
