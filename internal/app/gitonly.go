package app

import (
	"context"

	"viff/internal/domain"
	"viff/internal/gitcli"
)

// The helpers below reach Git-only services on the active adapter. vcs.Backend
// deliberately omits them: undo, LFS tracking and the remote HEAD name have no
// Subversion equivalent, so a backend that lacks them must not be forced to
// stub a method that returns nothing. A type assertion keeps them optional and
// lets each call site decide whether absence means "skip" (a refresh) or
// "refuse" (undo).

// headReader serves the identity query an undo record is built from.
type headReader interface {
	Head(ctx context.Context, repo domain.Repo) (gitcli.HeadState, error)
}

// remoteHeadReader names the branch a pull request targets.
type remoteHeadReader interface {
	RemoteHeads(ctx context.Context, repo domain.Repo) (map[string]string, error)
}

// HEAD is the commit and branch the undo history records before and after a
// write. A backend without a local HEAD (Subversion) reports ErrUnsupported,
// which is how undo stays off there.
func (w *Workspace) HEAD(ctx context.Context, repo domain.Repo) (gitcli.HeadState, error) {
	h, ok := w.Git.(headReader)
	if !ok {
		return gitcli.HeadState{}, ErrUnsupported
	}
	return h.Head(ctx, repo)
}

// hasUndo reports whether the active backend can record an operation that may
// later be reversed. It also covers the absence of a history writer, since
// reversing a recorded state is a history rewrite.
func (w *Workspace) hasUndo() bool {
	if _, ok := w.Git.(headReader); !ok {
		return false
	}
	return w.Caps.HistoryWriter != nil
}
