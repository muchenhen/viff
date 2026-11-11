package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"viff/internal/domain"
	"viff/internal/gitcli"
	"viff/internal/i18n"
)

// Op is a write command's body. progress reports Git's progress lines.
type Op func(ctx context.Context, repo domain.Repo, progress func(string)) error

// RunOptions tunes how a command reports.
type RunOptions struct {
	// Success is the notification title on success; "" shows none.
	Success string
	// SuccessBody is its body.
	SuccessBody string
	// OnError replaces the default error notification. It runs on the UI
	// thread and returns true when it handled the error.
	OnError func(error) bool
	// After runs on the UI thread after success.
	After func()
	// Always runs on the UI thread after the command, success or not.
	Always func()
	// Read marks a command that does not need the write lock.
	Read bool
}

// Run executes a write command as a task. Writes to one repository run one at
// a time; after any write, finished or failed or cancelled, the state is
// queried again from Git, because a cancelled write may have done part of
// its work.
func (w *Workspace) Run(title string, op Op, o RunOptions) {
	w.run(title, nil, op, o)
}

// run is Run plus the undo record a successful command hands to the session.
// The HEAD states are read from Git on either side of op while the write lock
// is held, so a failed or cancelled command records nothing and a successful
// record names the state the command really saw, not the view the user saw.
func (w *Workspace) run(title string, undo *undoSpec, op Op, o RunOptions) {
	rs := w.Repo
	if rs == nil {
		return
	}
	// The first remote version is an observation mode. A write never reaches
	// the server; the UI hides the actions, and this is the shared backstop for
	// a keyboard shortcut or a dialog that slipped through.
	if !o.Read && rs.Repo.IsSSH() {
		err := &gitcli.Error{Kind: gitcli.ErrReadOnlyRemote, Stderr: "remote repositories are read-only in Viff"}
		if o.OnError != nil {
			if !o.OnError(err) {
				w.NotifyError(w.tr(i18n.AppNoticeReadOnlyRemote), err)
			}
		} else {
			w.Notify(NoticeInfo, w.tr(i18n.AppNoticeReadOnlyRemote), w.tr(i18n.AppNoticeReadOnlyRemoteBody))
		}
		if o.Always != nil {
			o.Always()
		}
		return
	}
	task, ctx, finish := w.startTask(title, !o.Read)
	if !o.Read {
		rs.watchWrites++
	}
	if !o.Read && rs.LFSStats.Status == LFSStatsLoading {
		// Even a write that leaves the same final HEAD/index can race with
		// the scan. Its follow-up status query handles completed statistics.
		w.invalidateLFSStats(rs)
	}
	repo := rs.Repo
	go func() {
		var lock interface {
			Lock()
			Unlock()
		}
		if !o.Read {
			lock = writeLock(repo)
			lock.Lock()
		}
		var before gitcli.HeadState
		capture := false
		if undo != nil && w.hasUndo() {
			// An unborn branch has no commit to go back to, so it is not
			// recorded; undo needs a before state it can name.
			if hs, err := w.HEAD(ctx, repo); err == nil && hs.Head != "" {
				before, capture = hs, true
			}
		}
		err := op(ctx, repo, func(line string) { w.setProgress(task, line) })
		var entry *undoEntry
		if err == nil && capture && (!undo.kind.needsRef() || undo.hash != "") {
			if after, herr := w.HEAD(ctx, repo); herr == nil {
				entry = &undoEntry{
					kind: undo.kind, repoRoot: repo.Root, before: before, after: after,
					name: undo.name, oldName: undo.oldName, hash: undo.hash,
					previous: undo.previous, when: w.now(),
				}
			}
		}
		if lock != nil {
			lock.Unlock()
		}
		w.post(func() {
			finish()
			if !o.Read {
				rs.watchWrites--
			}
			if entry != nil {
				w.recordUndo(entry)
			}
			if w.Repo == rs {
				if !o.Read {
					rs.watchCovered = time.Now()
				}
				if rs.watchLost {
					rs.watchLost = false
					w.RefreshManually()
				} else {
					w.Refresh(true)
				}
			}
			if err != nil {
				if o.OnError == nil || !o.OnError(err) {
					w.NotifyError(w.tr(i18n.AppNoticeFailed, title), err)
				}
			} else {
				if o.Success != "" {
					w.Notify(NoticeSuccess, o.Success, o.SuccessBody)
				}
				if o.After != nil {
					o.After()
				}
			}
			if o.Always != nil {
				o.Always()
			}
		})
	}()
}

// StagePaths stages whole files.
func (w *Workspace) StagePaths(paths []string) {
	if len(paths) == 0 {
		return
	}
	w.Run("Stage", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		return w.Git.Stage(ctx, repo, paths)
	}, RunOptions{})
}

// UnstagePaths unstages whole files.
func (w *Workspace) UnstagePaths(paths []string) {
	if len(paths) == 0 || w.Repo == nil {
		return
	}
	hasHead := w.Repo.HasHead()
	w.Run("Unstage", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		return w.Git.Unstage(ctx, repo, paths, hasHead)
	}, RunOptions{})
}

// Rollback discards local changes of tracked files (from HEAD for staged
// ones) and deletes the given untracked files. The UI confirms first.
func (w *Workspace) Rollback(tracked, untracked []string, fromHead bool) {
	w.Run("Rollback", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		if len(tracked) > 0 {
			var err error
			if fromHead {
				err = w.Git.Revert(ctx, repo, tracked)
			} else {
				err = w.Git.Discard(ctx, repo, tracked)
			}
			if err != nil {
				return err
			}
		}
		for _, p := range untracked {
			full := filepath.Join(repo.Root, filepath.FromSlash(p))
			if !strings.HasPrefix(full, repo.Root+string(filepath.Separator)) {
				return fmt.Errorf("refusing to delete %s outside the repository", p)
			}
			if err := os.RemoveAll(full); err != nil {
				return err
			}
		}
		return nil
	}, RunOptions{})
}

// BlockAction is what to do with one changed block of a working diff.
type BlockAction uint8

const (
	BlockStage BlockAction = iota
	BlockUnstage
	BlockRevert // discard from the working tree
)

// ApplyBlock stages, unstages or reverts lines of a working-tree diff. sel
// selects the old-side and new-side lines of the block to apply (nil means
// the whole block). Before applying, both sides are read again: if either
// changed since the diff was shown, nothing is applied and ErrStale is
// returned through done, so an external tool's newer edit is never
// overwritten with an old patch.
func (w *Workspace) ApplyBlock(d *DiffResult, block int, action BlockAction, selOld, selNew func(int) bool, done func(error)) {
	if d == nil || !d.Source.Working || block < 0 || block >= len(d.Blocks) {
		return
	}
	b := d.Blocks[block]
	if selOld == nil {
		selOld = domain.SelectRange(b.OldStart, b.OldEnd)
	}
	if selNew == nil {
		selNew = domain.SelectRange(b.NewStart, b.NewEnd)
	}
	src := d.Source
	oldWant, newWant := d.Old.Content, d.New.Content
	oldSide := domain.PatchSide{Lines: d.Old.Lines, NoFinalNewline: d.Old.NoEOL}
	newSide := domain.PatchSide{Lines: d.New.Lines, NoFinalNewline: d.New.NoEOL}
	blocks := d.Blocks
	title := map[BlockAction]string{BlockStage: "Stage lines", BlockUnstage: "Unstage lines", BlockRevert: "Revert lines"}[action]
	w.Run(title, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		oldRev, newRev, _, _ := revisions(src, w.translator())
		oldNow, err := w.Git.Read(ctx, repo, oldRev, src.Path)
		if err != nil {
			return err
		}
		newNow, err := w.Git.Read(ctx, repo, newRev, src.Path)
		if err != nil {
			return err
		}
		if !bytes.Equal(oldNow.Data, oldWant.Data) || !bytes.Equal(newNow.Data, newWant.Data) || oldNow.Exists != oldWant.Exists || newNow.Exists != newWant.Exists {
			return ErrStale
		}
		var patch []byte
		cached := true
		switch {
		case action == BlockStage && src.Area == domain.AreaUnstaged:
			// index → worktree, applied forward to the index.
			patch = domain.BuildPatch(src.Path, oldSide, newSide, blocks, selOld, selNew)
		case action == BlockUnstage && src.Area == domain.AreaStaged:
			// HEAD → index: build index → HEAD for the selection and apply it
			// to the index.
			patch = domain.BuildPatch(src.Path, newSide, oldSide, swapBlocks(blocks), selNew, selOld)
		case action == BlockRevert && src.Area == domain.AreaUnstaged:
			// worktree → index for the selection, applied to the worktree.
			patch = domain.BuildPatch(src.Path, newSide, oldSide, swapBlocks(blocks), selNew, selOld)
			cached = false
		default:
			return fmt.Errorf("this action does not apply here")
		}
		if patch == nil {
			return nil
		}
		return w.Git.ApplyPatch(ctx, repo, patch, cached, false)
	}, RunOptions{OnError: func(err error) bool {
		if done != nil {
			done(err)
		}
		if err == ErrStale {
			w.Notify(NoticeWarning, w.tr(i18n.AppNoticeFileChanged), w.tr(i18n.AppNoticeFileChangedBody))
			return true
		}
		return false
	}, After: func() {
		if done != nil {
			done(nil)
		}
	}})
}

func swapBlocks(blocks []domain.Block) []domain.Block {
	out := make([]domain.Block, len(blocks))
	for i, b := range blocks {
		out[i] = domain.Block{OldStart: b.NewStart, OldEnd: b.NewEnd, NewStart: b.OldStart, NewEnd: b.OldEnd}
	}
	return out
}

// CommitRequest is the commit tool window's input.
type CommitRequest struct {
	Message  string
	Amend    bool
	SignOff  bool
	NoVerify bool
	// Paths is the target set of this commit. A backend without an index
	// (Subversion) has no other way to commit a subset: the paths the user
	// checked are what gets committed. Empty means every scheduled change.
	// A backend with an index ignores it and commits what is staged.
	Paths []string
	// Push pushes after a successful commit.
	Push bool
}

// Commit records the staged changes. Hooks run with the user's environment;
// a failing hook fails the commit and its output is shown.
func (w *Workspace) Commit(req CommitRequest, after func()) {
	staged, _, _, _ := w.Repo.Status.Counts()
	// What the notice counts is what was committed. Without an index the
	// staged count is always zero, so the notice would claim "0 files
	// committed" for every Subversion commit.
	committed := staged
	if !w.Caps.Index {
		committed = w.CommittableCount()
		if len(req.Paths) > 0 {
			committed = len(req.Paths)
		}
	}
	subject := strings.SplitN(strings.TrimSpace(req.Message), "\n", 2)[0]
	body := w.trPlural(committed, i18n.AppNoticeCommittedBody, i18n.AppNoticeCommittedBodyMany, committed, subject)
	if req.Amend {
		body = w.tr(i18n.AppNoticeAmendedBody, subject)
	}
	spec := &undoSpec{kind: undoCommit}
	if req.Amend {
		spec.kind = undoAmend
	}
	w.run("Commit", spec, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		_, err := w.Git.Commit(ctx, repo, gitcli.CommitOptions{Message: req.Message, Amend: req.Amend, SignOff: req.SignOff, NoVerify: req.NoVerify, Paths: req.Paths})
		return err
	}, RunOptions{Success: w.tr(i18n.AppNoticeCommitted), SuccessBody: body, After: after})
}

// LastMessage loads HEAD's message for amend.
func (w *Workspace) LastMessage(done func(string)) {
	rs := w.Repo
	if rs == nil || !rs.HasHead() {
		return
	}
	repo := rs.Repo
	go func() {
		msg, err := w.Git.LastCommitMessage(context.Background(), repo)
		w.post(func() {
			if err == nil && w.Repo == rs {
				done(msg)
			}
		})
	}()
}

// Checkout switches to a ref.
func (w *Workspace) Checkout(ref domain.Ref) {
	w.run("Checkout "+ref.Short, &undoSpec{kind: undoCheckout}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		return w.Git.Checkout(ctx, repo, ref)
	}, RunOptions{OnError: func(err error) bool {
		if w.checkoutBlocked(CheckoutBlocked{Ref: ref, Err: err}) {
			return true
		}
		return false
	}, Success: "", After: func() {
		w.Notify(NoticeInfo, w.tr(i18n.AppNoticeCheckedOut, ref.Short), "")
	}})
}

// CheckoutRevision detaches HEAD.
func (w *Workspace) CheckoutRevision(rev string) {
	w.run("Checkout "+domain.ShortHash(rev), &undoSpec{kind: undoCheckout}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		return w.Git.CheckoutRevision(ctx, repo, rev)
	}, RunOptions{OnError: func(err error) bool {
		return w.checkoutBlocked(CheckoutBlocked{Rev: rev, Err: err})
	}, Success: w.tr(i18n.AppNoticeCheckedOut, domain.ShortHash(rev)), SuccessBody: w.tr(i18n.AppNoticeHeadDetachedBody)})
}

// NewBranch creates a branch.
func (w *Workspace) NewBranch(name, start string, checkout, overwrite bool) {
	spec := &undoSpec{kind: undoCreateBranch, name: name}
	w.run("Create branch "+name, spec, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		branches, err := w.branches()
		if err != nil {
			return err
		}
		if overwrite {
			// Remember what the overwrite replaces so undo can put it back.
			spec.previous, _ = w.Git.ResolveRef(ctx, repo, "refs/heads/"+name)
		}
		if err := branches.CreateBranch(ctx, repo, name, start, checkout, overwrite); err != nil {
			return err
		}
		// Undo only deletes the branch while it still holds this commit; a
		// commit made on it afterwards must keep the branch alive.
		spec.hash, _ = w.Git.ResolveRef(ctx, repo, "refs/heads/"+name)
		return nil
	}, RunOptions{Success: w.tr(i18n.AppNoticeCreatedBranch, name)})
}

// RenameBranch renames a local branch.
func (w *Workspace) RenameBranch(old, name string) {
	spec := &undoSpec{kind: undoRenameBranch, name: name, oldName: old}
	w.run("Rename branch", spec, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		branches, err := w.branches()
		if err != nil {
			return err
		}
		if err := branches.RenameBranch(ctx, repo, old, name); err != nil {
			return err
		}
		// A rename keeps the commit, but recording it lets undo refuse when
		// the renamed branch moved in the meantime.
		spec.hash, _ = w.Git.ResolveRef(ctx, repo, "refs/heads/"+name)
		return nil
	}, RunOptions{Success: w.tr(i18n.AppNoticeRenamedBranch, old, name)})
}

// DeleteBranch deletes a branch. An unmerged branch is not deleted; the
// notification offers to force it, naming the consequence.
func (w *Workspace) DeleteBranch(ref domain.Ref, force bool) {
	if ref.Kind == domain.RefRemote {
		w.Run("Delete "+ref.Short, func(ctx context.Context, repo domain.Repo, progress func(string)) error {
			refs, err := w.refs()
			if err != nil {
				return err
			}
			return refs.DeleteRemoteBranch(ctx, repo, ref.Remote(), ref.BranchName(), progress)
		}, RunOptions{Success: w.tr(i18n.AppNoticeDeletedRemoteBranch, ref.Short)})
		return
	}
	target := ref.Target
	spec := &undoSpec{kind: undoDeleteBranch, name: ref.Short}
	w.run("Delete "+ref.Short, spec, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		branches, err := w.branches()
		if err != nil {
			return err
		}
		// Read the tip Git is about to delete, not the one the last refresh
		// showed: an external commit since then must be restorable too.
		sha, _ := w.Git.ResolveRef(ctx, repo, "refs/heads/"+ref.Short)
		if err := branches.DeleteBranch(ctx, repo, ref.Short, force); err != nil {
			return err
		}
		spec.hash = sha
		return nil
	}, RunOptions{
		OnError: func(err error) bool {
			if gitcli.KindOf(err) == gitcli.ErrNotMerged {
				w.Notify(NoticeWarning, w.tr(i18n.AppNoticeBranchNotMerged, ref.Short),
					w.tr(i18n.AppNoticeBranchNotMergedBody),
					NoticeAction{Title: w.tr(i18n.AppActionDeleteAnyway), Run: func() { w.DeleteBranch(ref, true) }})
				return true
			}
			return false
		},
		After: func() {
			w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeDeletedBranch, ref.Short), w.tr(i18n.AppNoticeDeletedBranchBody, domain.ShortHash(target)),
				NoticeAction{Title: w.tr(i18n.AppActionRestore), Run: func() { w.NewBranch(ref.Short, target, false, false) }})
		},
	})
}

// CreateTag creates a tag.
func (w *Workspace) CreateTag(name, rev, message string) {
	w.Run("Create tag "+name, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.CreateTag(ctx, repo, name, rev, message)
	}, RunOptions{Success: w.tr(i18n.AppNoticeCreatedTag, name)})
}

// DeleteTag deletes a local tag.
func (w *Workspace) DeleteTag(name string) {
	w.Run("Delete tag "+name, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.DeleteTag(ctx, repo, name)
	}, RunOptions{Success: w.tr(i18n.AppNoticeDeletedTag, name)})
}

// PushTag pushes a tag.
func (w *Workspace) PushTag(remote, name string) {
	w.Run("Push tag "+name, func(ctx context.Context, repo domain.Repo, p func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.PushTag(ctx, repo, remote, name, p)
	}, RunOptions{Success: w.tr(i18n.AppNoticePushedTag, name, remote)})
}

// DeleteRemoteTag deletes a tag on the remote. The local tag stays.
func (w *Workspace) DeleteRemoteTag(remote, name string) {
	w.Run("Delete remote tag "+name, func(ctx context.Context, repo domain.Repo, p func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.DeleteRemoteTag(ctx, repo, remote, name, p)
	}, RunOptions{Success: w.tr(i18n.AppNoticeDeletedRemoteTag, name, remote)})
}

// SetUpstream sets a branch's upstream.
func (w *Workspace) SetUpstream(branch, upstream string) {
	w.Run("Set upstream", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.SetUpstream(ctx, repo, branch, upstream)
	}, RunOptions{Success: w.tr(i18n.AppNoticeTrackingUpstream, upstream)})
}

// Fetch fetches all remotes with prune.
func (w *Workspace) Fetch(quiet bool) {
	if w.Repo == nil || len(w.Repo.Remotes) == 0 {
		if !quiet {
			w.Notify(NoticeInfo, w.tr(i18n.AppNoticeNothingToFetch), w.tr(i18n.AppNoticeNothingToFetchBody))
		}
		return
	}
	o := RunOptions{}
	if !quiet {
		o.Success = w.tr(i18n.AppNoticeFetchSuccessful)
	} else {
		o.OnError = func(error) bool { return true } // background fetch stays silent
	}
	w.Run("Fetching", func(ctx context.Context, repo domain.Repo, p func(string)) error {
		return w.Git.Fetch(ctx, repo, "", w.Settings == nil || w.Settings.FetchPrune, p)
	}, o)
}

// Pull updates the current branch from its upstream.
func (w *Workspace) Pull(mode gitcli.PullMode) {
	rs := w.Repo
	if rs == nil {
		return
	}
	if rs.Status.Branch.Upstream == "" {
		w.Notify(NoticeWarning, w.tr(i18n.AppNoticeCannotUpdate), w.tr(i18n.AppNoticeCannotUpdateBody))
		return
	}
	before := rs.Status.Branch.Head
	w.Run("Updating", func(ctx context.Context, repo domain.Repo, p func(string)) error {
		_, err := w.Git.Pull(ctx, repo, mode, p)
		return err
	}, RunOptions{
		OnError: func(err error) bool {
			if gitcli.KindOf(err) == gitcli.ErrConflict {
				w.Notify(NoticeWarning, w.tr(i18n.AppNoticeUpdateConflicts), w.tr(i18n.AppNoticeUpdateConflictsBody))
				return true
			}
			return false
		},
		After: func() {
			if w.Repo == nil {
				return
			}
			if w.Repo.Status.Branch.Head == before {
				w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeAllUpToDate), "")
			} else {
				w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeProjectUpdated), w.tr(i18n.AppNoticeProjectUpdatedBody, domain.ShortHash(w.Repo.Status.Branch.Head)))
			}
		},
	})
}

// UpdateBranch fast-forwards a local branch that no worktree has checked out
// from the remote's same-named branch. A branch that diverged, or that a
// worktree holds, is left untouched and explained instead of forced.
func (w *Workspace) UpdateBranch(remote, branch string) {
	var res gitcli.BranchUpdate
	w.Run("Updating "+branch, func(ctx context.Context, repo domain.Repo, p func(string)) error {
		var err error
		res, err = w.Git.UpdateBranch(ctx, repo, remote, branch, p)
		return err
	}, RunOptions{
		OnError: func(err error) bool {
			if errors.Is(err, gitcli.ErrBranchCheckedOut) {
				w.Notify(NoticeWarning, w.tr(i18n.AppNoticeCannotUpdateBranch, branch), w.tr(i18n.AppNoticeBranchCheckedOutBody))
				return true
			}
			if errors.Is(err, gitcli.ErrBranchDiverged) {
				w.Notify(NoticeWarning, w.tr(i18n.AppNoticeCannotFastForward, branch), w.tr(i18n.AppNoticeBranchDivergedBody))
				return true
			}
			return false
		},
		After: func() {
			switch {
			case res.UpToDate:
				w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeBranchUpToDate, branch), w.tr(i18n.AppNoticeBranchUpToDateBody, remote, branch))
			case res.New != "":
				w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeUpdatedBranch, branch), w.tr(i18n.AppNoticeUpdatedBranchBody, domain.ShortHash(res.Old), domain.ShortHash(res.New), res.Commits))
			}
		},
	})
}

// UnshallowRepository fetches the history a shallow clone left out.
func (w *Workspace) UnshallowRepository() {
	rs := w.Repo
	if rs == nil {
		return
	}
	if !rs.Shallow {
		w.Notify(NoticeInfo, w.tr(i18n.AppNoticeHistoryComplete), w.tr(i18n.AppNoticeHistoryCompleteBody))
		return
	}
	w.Run("Fetching full history", func(ctx context.Context, repo domain.Repo, p func(string)) error {
		return w.Git.Unshallow(ctx, repo, p)
	}, RunOptions{
		Success:     w.tr(i18n.AppNoticeFullHistoryFetched),
		SuccessBody: w.tr(i18n.AppNoticeFullHistoryFetchedBody),
	})
}

// Push pushes a branch.
func (w *Workspace) Push(o gitcli.PushOptions, after func()) {
	title := "Pushing"
	w.Run(title, func(ctx context.Context, repo domain.Repo, p func(string)) error {
		_, err := w.Git.Push(ctx, repo, o, p)
		return err
	}, RunOptions{
		OnError: w.pushError(o.ForceWithLease),
		After: func() {
			dst := o.RemoteName
			if dst == "" {
				dst = o.Branch
			}
			w.Notify(NoticeSuccess, w.tr(i18n.AppNoticePushedTo, o.Branch, o.Remote, dst), "")
			if after != nil {
				after()
			}
		},
	})
}

// pushError turns a rejected push into an actionable notification while
// keeping Git's own message; other errors keep the default handling.
func (w *Workspace) pushError(force bool) func(error) bool {
	return func(err error) bool {
		if gitcli.KindOf(err) != gitcli.ErrRejected {
			return false
		}
		msg := w.tr(i18n.AppNoticePushRejectedBody)
		if force {
			msg = w.tr(i18n.AppNoticePushRejectedStaleBody)
		}
		w.Notify(NoticeError, w.tr(i18n.AppNoticePushRejected), msg+"\n\n"+w.ErrorText(err),
			NoticeAction{Title: w.tr(i18n.AppActionUpdate), Run: func() { w.Pull(gitcli.PullMerge) }})
		return true
	}
}

// PushUpTo pushes rev and every commit before it to branch on remote. The
// history is sent as-is: a non-fast-forward update is rejected unless
// forceWithLease is set, and Git's own message is kept.
func (w *Workspace) PushUpTo(remote, branch, rev string, forceWithLease bool) {
	w.pushRef("Push up to "+domain.ShortHash(rev), remote, branch, rev, forceWithLease)
}

// PushHeadTo pushes HEAD to another remote or under another branch name.
func (w *Workspace) PushHeadTo(remote, branch string, forceWithLease bool) {
	w.pushRef("Push HEAD to "+remote+"/"+branch, remote, branch, "HEAD", forceWithLease)
}

func (w *Workspace) pushRef(title, remote, branch, src string, forceWithLease bool) {
	w.Run(title, func(ctx context.Context, repo domain.Repo, p func(string)) error {
		refs, rerr := w.refs()
		if rerr != nil {
			return rerr
		}
		return refs.PushRef(ctx, repo, remote, src, branch, forceWithLease, p)
	}, RunOptions{
		Success: w.tr(i18n.AppNoticePushedTo, pushSource(src), remote, branch),
		OnError: w.pushError(forceWithLease),
	})
}

func pushSource(src string) string {
	if src == "HEAD" {
		return "HEAD"
	}
	return domain.ShortHash(src)
}

// Outgoing lists the commits a push would send.
func (w *Workspace) Outgoing(branch, upstream string, done func([]domain.Commit, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		filter := gitcli.LogFilter{Revisions: []string{"refs/heads/" + branch}}
		if upstream != "" {
			filter.Revisions = append(filter.Revisions, "^refs/remotes/"+upstream)
		} else {
			// A new branch: what is not on any remote branch yet.
			filter.Revisions = append(filter.Revisions, "--not", "--remotes")
		}
		var commits []domain.Commit
		lr, err := w.Git.OpenLog(ctx, repo, filter)
		if err == nil {
			commits, _, err = lr.Next(500)
			lr.Close()
		}
		w.post(func() {
			if w.Repo == rs {
				done(commits, err)
			}
		})
	}()
}

// operationResult explains an operation that stopped with conflicts.
func (w *Workspace) operationError(title string) func(error) bool {
	return func(err error) bool {
		if gitcli.KindOf(err) == gitcli.ErrConflict {
			w.Notify(NoticeWarning, w.tr(i18n.AppNoticeStoppedWithConflicts, title),
				w.tr(i18n.AppNoticeStoppedWithConflictsBody))
			return true
		}
		return false
	}
}

// Merge merges rev into the current branch.
func (w *Workspace) Merge(rev string, noFF bool) {
	w.run("Merge "+rev, &undoSpec{kind: undoMerge}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.Merge(ctx, repo, rev, noFF, false)
		return err
	}, RunOptions{OnError: w.operationError("Merge"), Success: w.tr(i18n.AppNoticeMerged, rev)})
}

// Rebase rebases the current branch onto rev.
func (w *Workspace) Rebase(onto string) {
	w.run("Rebase onto "+onto, &undoSpec{kind: undoRebase}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.Rebase(ctx, repo, onto)
		return err
	}, RunOptions{OnError: w.operationError("Rebase"), Success: w.tr(i18n.AppNoticeRebasedOnto, onto)})
}

// CherryPick applies commits onto HEAD.
func (w *Workspace) CherryPick(hashes []string) {
	w.run("Cherry-pick", &undoSpec{kind: undoCherryPick}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.CherryPick(ctx, repo, hashes)
		return err
	}, RunOptions{OnError: w.operationError("Cherry-pick"), Success: w.trPlural(len(hashes), i18n.AppNoticeCherryPicked, i18n.AppNoticeCherryPickedMany, len(hashes))})
}

// RevertCommits reverts commits with new commits.
func (w *Workspace) RevertCommits(hashes []string) {
	w.run("Revert commits", &undoSpec{kind: undoRevert}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.RevertCommits(ctx, repo, hashes)
		return err
	}, RunOptions{OnError: w.operationError("Revert"), Success: w.trPlural(len(hashes), i18n.AppNoticeReverted, i18n.AppNoticeRevertedMany, len(hashes))})
}

// Reset moves the current branch.
func (w *Workspace) Reset(mode gitcli.ResetMode, rev string) {
	before := ""
	if w.Repo != nil {
		before = w.Repo.Status.Branch.Head
	}
	w.run("Reset", &undoSpec{kind: undoReset}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, err := w.history()
		if err != nil {
			return err
		}
		return h.Reset(ctx, repo, mode, rev)
	}, RunOptions{After: func() {
		w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeResetTo, domain.ShortHash(rev)), w.tr(i18n.AppNoticeResetBody, domain.ShortHash(before)),
			NoticeAction{Title: w.tr(i18n.AppActionCreateBranchAtOldPosition), Run: func() {
				w.NewBranch("backup-"+domain.ShortHash(before), before, false, false)
			}})
	}})
}

// OperationAction continues, skips or aborts the in-progress operation.
func (w *Workspace) OperationAction(action string) {
	rs := w.Repo
	if rs == nil || !rs.Operation.Active() {
		return
	}
	kind := rs.Operation.Kind
	title := strings.ToUpper(action[:1]) + action[1:] + " " + strings.ToLower(kind.Title())
	w.Run(title, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.OperationCommand(ctx, repo, kind, action)
		return err
	}, RunOptions{OnError: w.operationError(kind.Title())})
}

// StashPush stashes changes.
func (w *Workspace) StashPush(message string, untracked, keepIndex bool) {
	w.Run("Stash", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		s, err := w.stash()
		if err != nil {
			return err
		}
		return s.StashPush(ctx, repo, message, untracked, keepIndex)
	}, RunOptions{Success: w.tr(i18n.AppNoticeChangesStashed)})
}

// StashApply applies or pops a stash.
func (w *Workspace) StashApply(s domain.Stash, pop, index bool) {
	verb := "Apply"
	if pop {
		verb = "Pop"
	}
	w.Run(verb+" "+s.Ref, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		st, serr := w.stash()
		if serr != nil {
			return serr
		}
		_, err := st.StashApply(ctx, repo, s.Ref, pop, index)
		return err
	}, RunOptions{OnError: w.operationError("Unstash"), Success: w.tr(i18n.AppNoticeStashSucceeded, verb+" "+s.Ref)})
}

// StashDrop drops a stash.
func (w *Workspace) StashDrop(s domain.Stash) {
	hash := s.Hash
	w.Run("Drop "+s.Ref, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		st, err := w.stash()
		if err != nil {
			return err
		}
		return st.StashDrop(ctx, repo, s.Ref)
	}, RunOptions{Success: w.tr(i18n.AppNoticeStashDropped, s.Ref), SuccessBody: w.tr(i18n.AppNoticeStashDroppedBody, hash)})
}

// StashBranch creates a branch from a stash.
func (w *Workspace) StashBranch(s domain.Stash, branch string) {
	w.Run("Branch from stash", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		st, err := w.stash()
		if err != nil {
			return err
		}
		return st.StashBranch(ctx, repo, branch, s.Ref)
	}, RunOptions{Success: w.tr(i18n.AppNoticeCreatedBranchFromStash, branch, s.Ref)})
}

// AddRemote adds a remote.
func (w *Workspace) AddRemote(name, url string) {
	w.Run("Add remote", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.AddRemote(ctx, repo, name, url)
	}, RunOptions{Success: w.tr(i18n.AppNoticeAddedRemote, name)})
}

// RemoveRemote removes a remote.
func (w *Workspace) RemoveRemote(name string) {
	w.Run("Remove remote", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.RemoveRemote(ctx, repo, name)
	}, RunOptions{Success: w.tr(i18n.AppNoticeRemovedRemote, name)})
}

// SetRemoteURL edits a remote.
func (w *Workspace) SetRemoteURL(name, url string) {
	w.Run("Edit remote", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		refs, err := w.refs()
		if err != nil {
			return err
		}
		return refs.SetRemoteURL(ctx, repo, name, url)
	}, RunOptions{Success: w.tr(i18n.AppNoticeUpdatedRemote, name)})
}

// Reflog loads HEAD's reflog.
func (w *Workspace) Reflog(done func([]domain.ReflogEntry, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		r, rerr := w.reflog()
		if rerr != nil {
			w.post(func() {
				if w.Repo == rs {
					done(nil, rerr)
				}
			})
			return
		}
		list, err := r.Reflog(context.Background(), repo, 300)
		w.post(func() {
			if w.Repo == rs {
				done(list, err)
			}
		})
	}()
}

// Blame annotates a file at rev ("" for the working tree) in the background.
func (w *Workspace) Blame(rev, path string, done func(*domain.Blame, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		b, err := w.Git.Blame(ctx, repo, rev, path)
		w.post(func() {
			if w.Repo == rs {
				done(b, err)
			}
		})
	}()
}

// CreatePatch builds a patch for paths (all changes when empty) in the
// background. staged takes the index against HEAD; otherwise the working tree
// against the index. Paths are relative to the repository root.
func (w *Workspace) CreatePatch(paths []string, staged bool, done func([]byte, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		patch, err := w.Git.CreatePatch(ctx, repo, paths, staged)
		w.post(func() {
			if w.Repo == rs {
				done(patch, err)
			}
		})
	}()
}

// ApplyPatchFile applies a saved patch to the working tree or, with cached,
// to the index alone. Git checks the whole patch first, so a patch that does
// not apply leaves the repository untouched; the failure keeps Git's message.
func (w *Workspace) ApplyPatchFile(path string, cached bool, done func(error)) {
	body := filepath.Base(path)
	if cached {
		body += w.tr(i18n.AppNoticePatchAppliedToIndex)
	}
	w.Run("Apply Patch", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		patch, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read patch: %w", err)
		}
		return w.Git.ApplyPatch(ctx, repo, patch, cached, false)
	}, RunOptions{
		Success:     w.tr(i18n.AppNoticePatchApplied),
		SuccessBody: body,
		OnError: func(err error) bool {
			w.Notify(NoticeError, w.tr(i18n.AppNoticeCannotApplyPatch), w.ErrorText(err))
			if done != nil {
				done(err)
			}
			return true
		},
		After: func() {
			if done != nil {
				done(nil)
			}
		},
	})
}

// Worktrees loads the worktree list and remembers it in the repository state.
func (w *Workspace) Worktrees(done func([]domain.Worktree, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		wt, werr := w.worktrees()
		if werr != nil {
			w.post(func() {
				if w.Repo == rs && done != nil {
					done(nil, werr)
				}
			})
			return
		}
		list, err := wt.Worktrees(context.Background(), repo)
		w.post(func() {
			if w.Repo != rs {
				return
			}
			if err == nil {
				rs.Worktrees, rs.WorktreesLoaded = list, true
			}
			if done != nil {
				done(list, err)
			}
		})
	}()
}

// AddWorktree creates a worktree at path with a new branch, starting at
// start ("" means HEAD). It reports the git error, then reloads the state.
func (w *Workspace) AddWorktree(path, branch, start string, done func(error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	_, ctx, finish := w.startTask("New worktree "+branch, true)
	repo := rs.Repo
	go func() {
		lock := writeLock(repo)
		lock.Lock()
		err := w.addWorktree(ctx, repo, path, branch, start)
		lock.Unlock()
		w.post(func() {
			finish()
			if w.Repo == rs {
				if err != nil {
					w.NotifyError(w.tr(i18n.AppNoticeCannotCreateWorktree), err)
				} else {
					w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeWorktreeCreated), path)
					w.Refresh(true)
				}
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// AddWorktreeExisting checks out an existing local branch in a new linked
// worktree at path. Unlike AddWorktree it creates no branch; a missing or
// remote-only branch and a branch already checked out anywhere are refused
// (Git checks again while adding). It reports the git error, then refreshes.
func (w *Workspace) AddWorktreeExisting(path, branch string, done func(error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	_, ctx, finish := w.startTask("New worktree "+branch, true)
	repo := rs.Repo
	go func() {
		lock := writeLock(repo)
		lock.Lock()
		err := w.addWorktreeExisting(ctx, repo, path, branch)
		lock.Unlock()
		w.post(func() {
			finish()
			if w.Repo == rs {
				if err != nil {
					w.NotifyError(w.tr(i18n.AppNoticeCannotCreateWorktree), err)
				} else {
					w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeWorktreeCreated), path)
					w.Refresh(true)
				}
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// RemoveWorktree deletes the worktree at path. Without force Git refuses to
// delete one with modified or untracked files. The branch stays.
func (w *Workspace) RemoveWorktree(path string, force bool, done func(error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	_, ctx, finish := w.startTask("Delete worktree", true)
	repo := rs.Repo
	go func() {
		lock := writeLock(repo)
		lock.Lock()
		err := w.removeWorktree(ctx, repo, path, force)
		lock.Unlock()
		w.post(func() {
			finish()
			if w.Repo == rs {
				if err != nil {
					w.NotifyError(w.tr(i18n.AppNoticeCannotDeleteWorktree), err)
				} else {
					w.Notify(NoticeSuccess, w.tr(i18n.AppNoticeWorktreeDeleted), path)
					w.Refresh(true)
				}
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// Submodules lists the repository's submodules in the background.
func (w *Workspace) Submodules(done func([]domain.Submodule, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		sm, serr := w.submodules()
		if serr != nil {
			w.post(func() {
				if w.Repo == rs {
					done(nil, serr)
				}
			})
			return
		}
		list, err := sm.Submodules(context.Background(), repo)
		w.post(func() {
			if w.Repo == rs {
				done(list, err)
			}
		})
	}()
}

// UpdateSubmodule checks out the commit recorded in the index, initializing
// the submodule when init is set. An empty path updates every submodule.
func (w *Workspace) UpdateSubmodule(path string, init bool, done func(error)) {
	name := w.tr(i18n.AppNoticeAllSubmodules)
	if path != "" {
		name = path
	}
	var runErr error
	w.Run("Update "+name, func(ctx context.Context, repo domain.Repo, progress func(string)) error {
		sm, serr := w.submodules()
		if serr != nil {
			return serr
		}
		return sm.UpdateSubmodule(ctx, repo, path, init, progress)
	}, RunOptions{
		Success: w.tr(i18n.AppNoticeUpdatedSubmodule, name),
		OnError: func(err error) bool { runErr = err; return false },
		Always: func() {
			if done != nil {
				done(runErr)
			}
		},
	})
}

// SyncSubmodules copies the URLs from .gitmodules into .git/config.
func (w *Workspace) SyncSubmodules(path string, done func(error)) {
	name := w.tr(i18n.AppNoticeAllSubmodules)
	if path != "" {
		name = path
	}
	var runErr error
	w.Run("Sync "+name, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		sm, serr := w.submodules()
		if serr != nil {
			return serr
		}
		return sm.SyncSubmodules(ctx, repo, path)
	}, RunOptions{
		Success: w.tr(i18n.AppNoticeSyncedSubmodule, name),
		OnError: func(err error) bool { runErr = err; return false },
		Always: func() {
			if done != nil {
				done(runErr)
			}
		},
	})
}

// Clone clones into parent/dir and opens the result.
func (w *Workspace) Clone(url, parent, dir string, done func(error)) {
	task, ctx, finish := w.startTask("Cloning "+url, true)
	go func() {
		err := os.MkdirAll(parent, 0o755)
		if err == nil {
			err = w.Git.Clone(ctx, parent, url, dir, func(l string) { w.setProgress(task, l) })
		}
		w.post(func() {
			finish()
			if err != nil {
				w.NotifyError(w.tr(i18n.AppNoticeCloneFailed), err)
			} else {
				w.Open(filepath.Join(parent, dir), nil)
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// Init creates a repository and opens it.
//
// It always creates a Git repository, whatever the open working copy is: a
// Subversion working copy has no local repository to create, and the backend in
// front of the user must not decide what appears in an unrelated directory.
func (w *Workspace) Init(dir string, done func(error)) {
	_, ctx, finish := w.startTask("Init", true)
	go func() {
		b := w.gitBackend()
		var err error
		if b == nil {
			err = ErrUnsupported
		} else if err = os.MkdirAll(dir, 0o755); err == nil {
			err = b.Init(ctx, dir)
		}
		w.post(func() {
			finish()
			if err != nil {
				w.NotifyError(w.tr(i18n.AppNoticeInitFailed), err)
			} else {
				w.Open(dir, nil)
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// CompareWith loads the changes between two revisions (rev..HEAD style).
func (w *Workspace) CompareWith(from, to string, done func([]domain.Change, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		list, err := w.Git.DiffNames(context.Background(), repo, from, to)
		w.post(func() {
			if w.Repo == rs {
				done(list, err)
			}
		})
	}()
}

// CompareCommits loads the files that differ between two commits: from is the
// old side and to the new one. The commits need not be parent and child; any
// two commit-ish revisions work, and the order decides the direction.
func (w *Workspace) CompareCommits(from, to string, done func([]domain.Change, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		list, err := w.Git.DiffNames(ctx, repo, from, to)
		w.post(func() {
			if w.Repo == rs {
				done(list, err)
			}
		})
	}()
}

// RevisionTree lists the files in the tree of rev, sorted by path, in the
// background. The header of the browser comes from the log entry the UI
// already has, so only the tree is asked of Git.
func (w *Workspace) RevisionTree(rev string, done func([]domain.TreeEntry, error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		list, err := w.Git.TreeAt(ctx, repo, rev)
		w.post(func() {
			if w.Repo == rs {
				done(list, err)
			}
		})
	}()
}

// RebaseCommits loads the commits an interactive rebase starting at from
// would replay, oldest first, and the base it rebases onto.
func (w *Workspace) RebaseCommits(from string, done func(list []gitcli.RebaseCommit, base string, err error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		h, herr := w.history()
		if herr != nil {
			w.post(func() {
				if w.Repo == rs {
					done(nil, "", herr)
				}
			})
			return
		}
		list, base, err := h.RebaseCommits(ctx, repo, from)
		w.post(func() {
			if w.Repo == rs {
				done(list, base, err)
			}
		})
	}()
}

// InteractiveRebase replays the commits after base following the plan.
// Conflicts stop it like any rebase; the operation banner then offers
// Continue, Skip and Abort.
func (w *Workspace) InteractiveRebase(base string, steps []gitcli.RebaseStep) {
	w.run("Interactive rebase", &undoSpec{kind: undoRebase}, func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.InteractiveRebase(ctx, repo, base, steps)
		return err
	}, RunOptions{OnError: w.operationError("Rebase"), Success: w.tr(i18n.AppNoticeRebaseFinished)})
}

// CommitMessage loads one commit's full message for the edit dialog.
func (w *Workspace) CommitMessage(hash string, done func(message string, err error)) {
	rs := w.Repo
	if rs == nil {
		return
	}
	repo := rs.Repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		d, err := w.Git.CommitDetail(ctx, repo, hash)
		w.post(func() {
			if w.Repo == rs {
				done(d.Body, err)
			}
		})
	}()
}

// RewordCommit replaces one commit's message. Its content and the commits
// after it stay the same.
func (w *Workspace) RewordCommit(hash, message string) {
	short := domain.ShortHash(hash)
	w.Run("Edit commit message", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.RewordCommit(ctx, repo, hash, message)
		return err
	}, RunOptions{OnError: w.rewriteError("Edit commit message"), Success: w.tr(i18n.AppNoticeMessageUpdated, short),
		SuccessBody: w.tr(i18n.AppNoticeMessageUpdatedBody)})
}

// SquashCommit folds one commit into its parent with a combined message.
func (w *Workspace) SquashCommit(hash, message string) {
	short := domain.ShortHash(hash)
	w.Run("Squash commit", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.SquashCommit(ctx, repo, hash, message)
		return err
	}, RunOptions{OnError: w.rewriteError("Squash commit"), Success: w.tr(i18n.AppNoticeSquashedIntoParent, short),
		SuccessBody: w.tr(i18n.AppNoticeSquashedBody)})
}

// DropCommit removes one commit from the branch; its changes disappear.
func (w *Workspace) DropCommit(hash string) {
	short := domain.ShortHash(hash)
	w.Run("Drop commit", func(ctx context.Context, repo domain.Repo, _ func(string)) error {
		h, herr := w.history()
		if herr != nil {
			return herr
		}
		_, err := h.DropCommit(ctx, repo, hash)
		return err
	}, RunOptions{OnError: w.rewriteError("Drop commit"), Success: w.tr(i18n.AppNoticeDroppedCommit, short),
		SuccessBody: w.tr(i18n.AppNoticeDroppedCommitBody)})
}

// rewriteError explains a refused or aborted history edit. The Git command
// aborts its own rebase, so the branch is back where it started.
func (w *Workspace) rewriteError(title string) func(error) bool {
	return func(err error) bool {
		switch {
		case errors.Is(err, gitcli.ErrRewriteAbortLeft):
			w.Notify(NoticeError, w.tr(i18n.AppNoticeFailed, title), w.tr(i18n.AppNoticeRewriteAbortLeftBody, w.ErrorText(err)))
			return true
		case gitcli.KindOf(err) == gitcli.ErrConflict:
			w.Notify(NoticeWarning, w.tr(i18n.AppNoticeWasAborted, title), w.tr(i18n.AppNoticeEditHitConflictBody))
			return true
		case errors.Is(err, gitcli.ErrRewriteOperation),
			errors.Is(err, gitcli.ErrRewriteRoot),
			errors.Is(err, gitcli.ErrRewriteMerge),
			errors.Is(err, gitcli.ErrRewriteUnrelated),
			errors.Is(err, gitcli.ErrRewriteMessage):
			w.Notify(NoticeWarning, title, err.Error())
			return true
		}
		return false
	}
}
