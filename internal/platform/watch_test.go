package platform

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"viff/internal/domain"
	"viff/internal/gitcli"
)

func watchGitFixture(t *testing.T) (*gitcli.Runner, domain.Repo) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	git := &gitcli.Runner{Env: []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
	}}
	watchGit(t, git, dir, "init", "-q", "-b", "main")
	watchGit(t, git, dir, "config", "user.name", "Viff Test")
	watchGit(t, git, dir, "config", "user.email", "test@example.com")
	repo, err := git.Discover(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return git, repo
}

func watchGit(t *testing.T, git *gitcli.Runner, root string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := git.Run(ctx, root, args...); err != nil {
		t.Fatal(err)
	}
}

func TestClassify(t *testing.T) {
	repo := domain.Repo{Root: "/r", MetaDir: "/r/.git", CommonDir: "/r/.git"}
	cases := []struct {
		path           string
		relevant, meta bool
	}{
		{"/r/a.txt", true, false},
		{"/r/.git/HEAD", true, true},
		{"/r/.git/refs/heads/main", true, true},
		{"/r/.git/objects/ab/cdef", false, false},
		{"/r/.git/index.lock", false, false},
	}
	for _, c := range cases {
		r, m := classify(repo, c.path)
		if r != c.relevant || m != c.meta {
			t.Errorf("%s = %v %v", c.path, r, m)
		}
	}
}

func TestWatchReportsChangesOnce(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metaDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{Root: dir, MetaDir: metaDir, CommonDir: metaDir}
	var worktree, meta atomic.Int32
	w, err := Watch(repo, func(change domain.RepoChange) {
		if change.Meta {
			meta.Add(1)
		} else {
			worktree.Add(1)
		}
	}, WatchOptions{})
	if err != nil {
		t.Skip("native watcher unavailable:", err)
	}
	defer w.Close()
	time.Sleep(300 * time.Millisecond)
	// A burst of writes is coalesced.
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i%26))), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The polling backend scans the worktree every thirty seconds (ADR 0003).
	wait := 5 * time.Second
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		wait = 35 * time.Second
	}
	deadline := time.Now().Add(wait)
	for worktree.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if worktree.Load() == 0 {
		t.Fatal("no worktree event")
	}
	time.Sleep(600 * time.Millisecond)
	if n := worktree.Load(); n > 3 {
		t.Fatalf("burst produced %d callbacks", n)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(wait)
	for meta.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if meta.Load() == 0 {
		t.Fatal("no meta event")
	}
}
