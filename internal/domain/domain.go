// Package domain holds Viff's Git domain model: repository identity, refs,
// commits, working-tree status, diffs, conflicts and the in-progress operation
// state. It imports neither the UI framework nor os/exec, so every rule here
// is testable as plain data.
package domain

import (
	"path"
	"strings"
	"time"
)

// Kind names the version control system a repository belongs to. It lives in
// domain so snapshots and caches can carry it without importing an adapter.
type Kind uint8

const (
	KindGit Kind = iota
	KindSVN
)

// Repo identifies one working tree. MetaDir is the backend's own metadata
// directory (.git or .svn); CommonDir differs from MetaDir in a linked Git
// worktree, where refs and objects live in CommonDir.
//
// URL, RelURL, RepoRoot and UUID identify a Subversion working copy. Git
// resolves everything from the local object store, but a Subversion working
// copy records only its own base revision, so its repository identity has to
// travel with it; UUID plus RelURL also survive moving the working copy, which
// a path does not.
type Repo struct {
	Root      string
	Kind      Kind
	MetaDir   string
	CommonDir string

	URL      string // working copy root's repository URL (Subversion)
	RelURL   string // repository-relative URL, "^/trunk" (Subversion)
	RepoRoot string // repository root URL (Subversion)
	UUID     string // repository UUID (Subversion)
}

// RepoChange is a hint to requery Git, not a snapshot of repository state.
// Observed is the newest observation in the batch, when the watcher knows it.
// Overflow means paths may have been lost and caches need revalidation.
type RepoChange struct {
	Observed time.Time
	Meta     bool
	Overflow bool
}

// LockKey is the serialization key for writes. Two working trees of one Git
// repository share refs and objects through CommonDir, so they must not write
// concurrently; Subversion has no shared metadata between checkouts, so each
// working copy is its own key.
func (r Repo) LockKey() string {
	if r.Kind == KindSVN {
		return r.Root
	}
	if r.CommonDir != "" {
		return r.CommonDir
	}
	return r.MetaDir
}

// Name is the folder name shown as the project name.
func (r Repo) Name() string {
	root := strings.TrimRight(strings.ReplaceAll(r.Root, "\\", "/"), "/")
	if i := strings.LastIndex(root, "/"); i >= 0 {
		return root[i+1:]
	}
	return root
}

// IsLinkedWorktree reports a worktree created with git worktree add.
func (r Repo) IsLinkedWorktree() bool {
	return r.Kind == KindGit && r.CommonDir != "" && r.CommonDir != r.MetaDir
}

// IsSSH reports a repository Viff opens on another machine over SSH. The
// working tree is read-only in this version: Git runs on the server, and every
// command that would change its state is refused.
func (r Repo) IsSSH() bool { return strings.HasPrefix(r.Root, "ssh://") }

// RefKind classifies a reference.
type RefKind uint8

const (
	RefLocal RefKind = iota
	RefRemote
	RefTag
	RefHead
	RefStash
)

// Ref is one branch or tag. Name is the full name (refs/heads/main), Short
// the display name (main, origin/main, v1.0).
type Ref struct {
	Name     string
	Short    string
	Kind     RefKind
	Target   string // the commit the ref points at (peeled for annotated tags)
	Upstream string // short name of the upstream branch, local branches only
	Ahead    int
	Behind   int
	Gone     bool // upstream configured but no longer exists
	Current  bool
	Date     time.Time
	Subject  string
}

// Remote returns the remote part of a remote branch's short name.
func (r Ref) Remote() string {
	if r.Kind != RefRemote {
		return ""
	}
	if i := strings.IndexByte(r.Short, '/'); i > 0 {
		return r.Short[:i]
	}
	return r.Short
}

// BranchName returns the branch name without the remote prefix.
func (r Ref) BranchName() string {
	if r.Kind == RefRemote {
		if i := strings.IndexByte(r.Short, '/'); i > 0 {
			return r.Short[i+1:]
		}
	}
	return r.Short
}

// Remote is one configured remote.
type Remote struct {
	Name     string
	FetchURL string
	PushURL  string
}

// Commit is the metadata of one commit as listed in the log.
type Commit struct {
	Hash        string
	Parents     []string
	Author      string
	AuthorEmail string
	AuthorTime  time.Time
	Committer   string
	CommitTime  time.Time
	Subject     string
}

// ShortHash is the abbreviated hash shown in the UI. Object IDs are not
// assumed to be SHA-1 length.
func ShortHash(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// CommitDetail is the full message and change list of a selected commit.
type CommitDetail struct {
	Commit
	Body    string
	Changes []Change
}

// ChangeKind is a file's change between two trees.
type ChangeKind uint8

const (
	ChangeModified ChangeKind = iota
	ChangeAdded
	ChangeDeleted
	ChangeRenamed
	ChangeCopied
	ChangeTypeChanged
	ChangeUntracked
	ChangeConflicted
	ChangeIgnored
)

// Letter is the one-letter Git code of a change.
func (k ChangeKind) Letter() string {
	switch k {
	case ChangeAdded:
		return "A"
	case ChangeDeleted:
		return "D"
	case ChangeRenamed:
		return "R"
	case ChangeCopied:
		return "C"
	case ChangeTypeChanged:
		return "T"
	case ChangeUntracked:
		return "?"
	case ChangeConflicted:
		return "U"
	case ChangeIgnored:
		return "!"
	}
	return "M"
}

// Change is one file changed in a commit or between two revisions.
type Change struct {
	Kind    ChangeKind
	Path    string
	OldPath string // set for renames and copies
}

// FileName is the last path element.
func FileName(p string) string { return path.Base(p) }

// DirName is the parent directory, "" at the root.
func DirName(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// Area is where a working-tree entry is shown in the commit tool window.
type Area uint8

const (
	AreaStaged Area = iota
	AreaUnstaged
	AreaUntracked
	AreaConflicts
)

// StatusEntry is one path from a status query. Index and Worktree are the X
// and Y status letters ('.' for unchanged), Git's two-column form. A backend
// with a single change state (Subversion) reports Index '.' and puts the
// change in Worktree.
type StatusEntry struct {
	Path     string
	OrigPath string
	Index    byte
	Worktree byte
	// Props is a property change ('M', 'C' or ' '). Git has no versioned
	// properties and leaves it blank; Subversion reports svn:* changes
	// separately from text changes.
	Props byte
	// Submodule is the porcelain v2 submodule field ("N..." when not one).
	Submodule string
	ModeHead  string
	ModeIndex string
	ModeWork  string
}

// Untracked reports a "?" entry. Git puts the letter in both columns; a
// single-column backend only fills Worktree.
func (e StatusEntry) Untracked() bool { return e.Index == '?' || e.Worktree == '?' }

// Ignored reports a "!" entry.
func (e StatusEntry) Ignored() bool { return e.Index == '!' || e.Worktree == '!' }

// Conflicted reports an unmerged entry: any U, Git's AA/DD, or Subversion's
// single-column "conflicted" item.
func (e StatusEntry) Conflicted() bool {
	x, y := e.Index, e.Worktree
	return x == 'U' || y == 'U' || x == 'C' || y == 'C' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D')
}

// Staged reports changes recorded in the index relative to HEAD.
func (e StatusEntry) Staged() bool {
	return !e.Conflicted() && !e.Untracked() && !e.Ignored() && e.Index != '.' && e.Index != ' '
}

// Unstaged reports changes in the working tree relative to the index.
//
// A property-only change counts: Subversion reports it as item="normal" with
// props="modified", so the text column is blank while the working copy really
// differs from the base. Without that case the entry lands in no area at all
// and a file whose only change is svn:eol-style or svn:ignore disappears from
// the changes list, which also keeps it out of the next commit.
func (e StatusEntry) Unstaged() bool {
	if e.Conflicted() || e.Untracked() || e.Ignored() {
		return false
	}
	if e.Worktree != '.' && e.Worktree != ' ' {
		return true
	}
	return e.Props != 0 && e.Props != ' '
}

// IsSubmodule reports a submodule entry.
func (e StatusEntry) IsSubmodule() bool { return len(e.Submodule) > 0 && e.Submodule[0] == 'S' }

// KindIn returns the change kind of the entry as seen in area.
func (e StatusEntry) KindIn(area Area) ChangeKind {
	var c byte
	switch area {
	case AreaStaged:
		c = e.Index
	case AreaUnstaged:
		c = e.Worktree
	case AreaUntracked:
		return ChangeUntracked
	case AreaConflicts:
		return ChangeConflicted
	}
	switch c {
	case 'A':
		return ChangeAdded
	case 'D':
		return ChangeDeleted
	case 'R':
		return ChangeRenamed
	case 'C':
		return ChangeCopied
	case 'T':
		return ChangeTypeChanged
	}
	return ChangeModified
}

// BranchStatus is the "# branch" header of git status.
type BranchStatus struct {
	Head     string // commit id, "" before the first commit
	Branch   string // "" when detached
	Detached bool
	Upstream string
	Ahead    int
	Behind   int
	HasAB    bool
	Stashes  int
}

// Status is one git status snapshot.
type Status struct {
	Branch  BranchStatus
	Entries []StatusEntry
	// IndexKey identifies staged changes, including object changes that leave
	// the staged path, mode and status letter unchanged.
	IndexKey [32]byte
}

// Counts returns the number of entries in each area.
func (s Status) Counts() (staged, unstaged, untracked, conflicts int) {
	for _, e := range s.Entries {
		switch {
		case e.Conflicted():
			conflicts++
		case e.Untracked():
			untracked++
		case e.Ignored():
		default:
			if e.Staged() {
				staged++
			}
			if e.Unstaged() {
				unstaged++
			}
		}
	}
	return
}

// Clean reports a working tree without any changes.
func (s Status) Clean() bool {
	for _, e := range s.Entries {
		if !e.Ignored() {
			return false
		}
	}
	return true
}

// Stash is one stash entry.
type Stash struct {
	Index   int
	Ref     string // stash@{0}
	Hash    string
	Message string
	Time    time.Time
}

// ReflogEntry is one entry of HEAD's reflog.
type ReflogEntry struct {
	Hash     string
	Selector string // HEAD@{3}
	Message  string
	Time     time.Time
}

// Worktree is one entry of git worktree list.
type Worktree struct {
	Path     string
	Head     string
	Branch   string
	Bare     bool
	Detached bool
	Locked   bool
	Prunable bool
}
