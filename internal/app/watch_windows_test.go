package app

import (
	"context"
	"testing"

	"viff/internal/domain"
	"viff/internal/gitcli"
	"viff/internal/platform"
)

func TestWindowsNativeWatcherPublishesExternalGitState(t *testing.T) {
	f := newFixture(t)
	f.write("tracked.txt", "base\n")
	f.commitAll("base")
	f.git("branch", "side")
	f.write("tracked.txt", "main\n")
	f.commitAll("main edit")
	git := f.w.Git.(*gitcli.Runner)
	f.w.Watch = func(repo domain.Repo, changed func(domain.RepoChange)) (Watcher, error) {
		return platform.Watch(repo, changed, platform.WatchOptions{
			WorktreeRelevant: func(ctx context.Context, paths []string) (bool, error) {
				return git.WorktreeChangesRelevant(ctx, repo, paths)
			},
		})
	}
	f.open()
	f.git("checkout", "-q", "side")
	f.settle(func() bool { return f.idle() && f.w.Repo.CurrentBranch() == "side" })

	f.write("tracked.txt", "side\n")
	f.git("add", "tracked.txt")
	f.settle(func() bool { return f.idle() && len(f.w.Repo.Entries(domain.AreaStaged)) == 1 })
	f.git("commit", "-qm", "side edit")
	head := f.head()
	f.settle(func() bool { return f.idle() && f.w.Repo.Status.Branch.Head == head })

	if _, err := git.Run(context.Background(), f.dir, "merge", "main"); err == nil {
		t.Fatal("expected an external merge conflict")
	}
	f.settle(func() bool {
		rs := f.w.Repo
		return f.idle() && rs.Operation.Kind == domain.OpMerge && len(rs.Entries(domain.AreaConflicts)) == 1
	})
	f.git("merge", "--abort")
	f.settle(func() bool {
		rs := f.w.Repo
		return f.idle() && rs.Operation.Kind == domain.OpNone && len(rs.Entries(domain.AreaConflicts)) == 0
	})
}
