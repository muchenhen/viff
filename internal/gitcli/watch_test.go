package gitcli

import (
	"context"
	"testing"
)

func TestWorktreeChangesRelevantUsesIgnoreAndIndex(t *testing.T) {
	tr := newTestRepo(t)
	tr.write(".gitignore", "/build/\n*.tmp\n!keep.tmp\n.gitignore\n.gitattributes\n")
	tr.write(".gitattributes", "*.bin filter=lfs\n")
	tr.write("build/tracked.txt", "tracked")
	tr.write("build/generated [1].bin", "generated")
	tr.write("keep.tmp", "visible exception")
	tr.write("elsewhere/build/visible.txt", "same directory name")
	tr.git("add", "-f", "build/tracked.txt")
	tr.git("config", "core.ignorecase", "true")

	for _, test := range []struct {
		name  string
		paths []string
		want  bool
	}{
		{"ignored generated files", []string{"build/generated [1].bin", "build/new.tmp", "removed.tmp"}, false},
		{"tracked under ignore", []string{"build/tracked.txt"}, true},
		{"directory with tracked descendants", []string{"build"}, true},
		{"tracked case alias", []string{"BUILD/TRACKED.TXT"}, true},
		{"directory case alias", []string{"BUILD/"}, true},
		{"negated pattern", []string{"keep.tmp"}, true},
		{"same name outside ignored root", []string{"elsewhere/build/visible.txt"}, true},
		{"mixed batch", []string{"build/generated [1].bin", "keep.tmp"}, true},
		{"ignored rule source", []string{".gitignore"}, true},
		{"ignored LFS rule source", []string{".gitattributes"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := tr.r.WorktreeChangesRelevant(context.Background(), tr.repo, test.paths)
			if err != nil || got != test.want {
				t.Fatalf("relevant = %v, %v; want %v", got, err, test.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.r.WorktreeChangesRelevant(ctx, tr.repo, []string{"build/generated [1].bin"}); err == nil {
		t.Fatal("canceled ignore probe reported a result")
	}
}
