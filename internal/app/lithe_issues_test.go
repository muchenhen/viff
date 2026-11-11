package app

// Regression coverage for Git issues reported against Lithe (1lck/Lithe-IDEA),
// reproduced against Viff's application layer with real temporary repositories.
// These tests diagnose behaviour only; they do not change product code.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"viff/internal/domain"
)

// fakeWatcher stands in for platform.Watch so a test can deliver the same
// callback the OS watcher would send, without opening a window.
type fakeWatcher struct {
	changed func(domain.RepoChange)
	closed  bool
}

func (f *fakeWatcher) Close() { f.closed = true }

// Lithe #22: a commit made in a terminal changes HEAD, refs and the index.
// Those are Git-metadata events (meta=true), so the watcher refresh must reload
// the log and refs, not only the working-tree status. The existing platform
// test covers event classification; this covers the application half.
func TestIssue22ExternalCommitAndRefsRefreshThroughWatcher(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "one\n")
	f.commitAll("base")

	var watch *fakeWatcher
	f.w.Watch = func(repo domain.Repo, changed func(domain.RepoChange)) (Watcher, error) {
		watch = &fakeWatcher{changed: changed}
		return watch, nil
	}
	f.open()
	if watch == nil {
		t.Fatal("opening the repository did not start a watcher")
	}

	// In a terminal: commit and create a branch. No app command runs.
	f.write("a.txt", "two\n")
	f.git("commit", "-q", "-am", "external commit")
	f.git("branch", "external-branch")
	newHead := f.head()

	watch.changed(domain.RepoChange{Meta: true})
	f.settle(func() bool {
		rs := f.w.Repo
		return f.idle() && rs.Status.Branch.Head == newHead &&
			len(rs.Log.Commits) > 0 && rs.Log.Commits[0].Hash == newHead
	})

	rs := f.w.Repo
	if !rs.RefsLoaded {
		t.Fatal("refs were not reloaded after a metadata event")
	}
	var external bool
	for _, r := range rs.RefsOf(domain.RefLocal) {
		if r.Short == "external-branch" {
			external = r.Target == newHead
		}
	}
	if !external {
		t.Fatalf("new external branch missing after refresh: %+v", rs.RefsOf(domain.RefLocal))
	}
	if got := rs.Log.Commits[0].Subject; got != "external commit" {
		t.Fatalf("log top = %q, want external commit", got)
	}
	if len(rs.Entries(domain.AreaUnstaged)) != 0 || len(rs.Entries(domain.AreaStaged)) != 0 {
		t.Fatalf("status was not refreshed: %+v", rs.Status.Entries)
	}

	f.w.Close()
	if !watch.closed {
		t.Fatal("closing the repository did not close the watcher")
	}
}

// Lithe #560: reverting a commit from the Log must make the new revert commit
// appear in the Log without pressing the refresh button. Run() refreshes with
// meta=true; the refs fingerprint change has to reload the log.
func TestIssue560RevertRefreshesLogAutomatically(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a\n")
	f.commitAll("first")
	f.write("b.txt", "b\n")
	f.commitAll("second")
	f.write("c.txt", "c\n")
	f.commitAll("third")
	f.open()

	second := strings.TrimSpace(f.git("rev-parse", "HEAD~1"))
	f.w.RevertCommits([]string{second})
	f.settle(func() bool {
		l := f.w.Repo.Log
		return f.idle() && len(l.Commits) == 4 && l.Commits[0].Subject == `Revert "second"`
	})

	rs := f.w.Repo
	if rs.Log.Commits[0].Hash != rs.Status.Branch.Head {
		t.Fatal("revert commit is not at HEAD")
	}
	if n := strings.Count(strings.Join(logSubjects(rs.Log.Commits), "\n"), `Revert "second"`); n != 1 {
		t.Fatalf("revert commit count = %d", n)
	}
	if got := strings.TrimSpace(f.git("log", "-1", "--format=%s")); got != `Revert "second"` {
		t.Fatalf("git log = %q", got)
	}
}

func logSubjects(commits []domain.Commit) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.Subject
	}
	return out
}

// Lithe #764: a branch whose name is a prefix of another branch disappeared.
// Viff keeps refs by exact name; both local and remote same-prefix branches
// must survive listing, hash indexing and the exact log branch filter.
func TestIssue764SamePrefixBranchesStayDistinct(t *testing.T) {
	f := newFixture(t)
	f.write("base.txt", "base\n")
	f.commitAll("base")

	f.git("checkout", "-q", "-b", "release")
	f.write("release.txt", "release\n")
	f.commitAll("release only")
	releaseTip := f.head()

	f.git("checkout", "-q", "-b", "release-1", "main")
	f.write("release-1.txt", "one\n")
	f.commitAll("release-1 only")
	release1Tip := f.head()

	f.git("checkout", "-q", "main")
	f.git("update-ref", "refs/remotes/origin/release", releaseTip)
	f.git("update-ref", "refs/remotes/origin/release-1", release1Tip)
	f.open()

	rs := f.w.Repo
	locals := map[string]string{}
	for _, r := range rs.RefsOf(domain.RefLocal) {
		locals[r.Short] = r.Target
	}
	if locals["release"] != releaseTip || locals["release-1"] != release1Tip {
		t.Fatalf("local same-prefix branches = %+v", locals)
	}

	remotes := map[string]string{}
	for _, r := range rs.RefsOf(domain.RefRemote) {
		remotes[r.BranchName()] = r.Target
	}
	if remotes["release"] != releaseTip || remotes["release-1"] != release1Tip || len(remotes) != 2 {
		t.Fatalf("remote same-prefix branches = %+v", remotes)
	}

	if !hasRef(rs.RefsByHash[releaseTip], "refs/heads/release") ||
		!hasRef(rs.RefsByHash[release1Tip], "refs/heads/release-1") {
		t.Fatalf("same-prefix refs collided in RefsByHash: %v vs %v",
			rs.RefsByHash[releaseTip], rs.RefsByHash[release1Tip])
	}

	// The Branch filter passes the short name straight to git log; it must be
	// resolved as one revision, not as a prefix that pulls in both branches.
	f.w.SetLogFilter(LogFilter{Branch: "release"})
	f.settle(func() bool { return f.idle() && f.w.Repo.Log.Done })
	if subjects := logSubjects(f.w.Repo.Log.Commits); !equalSet(subjects, []string{"release only", "base"}) {
		t.Fatalf("filter release = %v", subjects)
	}

	f.w.SetLogFilter(LogFilter{Branch: "release-1"})
	f.settle(func() bool { return f.idle() && f.w.Repo.Log.Done })
	if subjects := logSubjects(f.w.Repo.Log.Commits); !equalSet(subjects, []string{"release-1 only", "base"}) {
		t.Fatalf("filter release-1 = %v", subjects)
	}
}

func hasRef(refs []domain.Ref, name string) bool {
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// Lithe #1662 (verified-bug context): a failed status read must not discard the
// last confirmed snapshot. Viff keeps Status/StatusLoaded/StatusTime and shows
// an error; the next successful read replaces it.
func TestIssue1662StatusFailureKeepsConfirmedSnapshot(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "one\n")
	f.commitAll("base")
	// A different length keeps Git from trusting a stale stat cache: with the
	// same size and a restored index, Windows Git can consider the file clean
	// (racy-clean) without reading its content.
	f.write("tracked.txt", "two changed\n")
	f.open()

	before := f.w.Repo.Status
	beforeTime := f.w.Repo.StatusTime
	beforeNotices := len(f.w.Notifications)
	if !f.w.Repo.StatusLoaded || len(before.Entries) != 1 {
		t.Fatalf("confirmed snapshot = %+v", before)
	}

	index := filepath.Join(f.dir, ".git", "index")
	indexInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte("garbage index"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.w.Refresh(true)
	f.settle(func() bool { return len(f.errorsSince(beforeNotices)) > 0 && f.idle() })

	rs := f.w.Repo
	if !rs.StatusLoaded {
		t.Fatal("status read failure cleared StatusLoaded")
	}
	if !reflect.DeepEqual(rs.Status.Entries, before.Entries) || rs.Status.Branch.Head != before.Branch.Head {
		t.Fatalf("status read failure replaced confirmed data:\n got %+v\nwant %+v", rs.Status, before)
	}
	if !rs.StatusTime.Equal(beforeTime) {
		t.Fatalf("status read failure moved StatusTime: %v -> %v", beforeTime, rs.StatusTime)
	}
	errs := f.errorsSince(beforeNotices)
	if errs[0].Title != "Cannot read status" {
		t.Fatalf("error notification = %+v", errs[0])
	}

	// Repairing the index lets the next refresh confirm the same state again.
	if err := os.WriteFile(index, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	// Writing the index moves its mtime; put it back so Git's racy-clean
	// bookkeeping sees the same picture as before the corruption.
	if err := os.Chtimes(index, indexInfo.ModTime(), indexInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Prove with Git itself that the fixture is still really dirty before
	// asking the app to recover the snapshot.
	if out := f.git("status", "--porcelain"); !strings.Contains(out, "tracked.txt") {
		t.Fatalf("fixture is not dirty after repairing the index:\n%s", out)
	}
	f.w.Refresh(true)
	f.settle(func() bool { return f.idle() && f.w.Repo.StatusLoaded })
	if !reflect.DeepEqual(f.w.Repo.Status.Entries, before.Entries) {
		t.Fatalf("recovered status = %+v", f.w.Repo.Status.Entries)
	}
}
