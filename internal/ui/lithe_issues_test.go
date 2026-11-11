//go:build vivahead && litheaudit

package viffui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	vivaapp "github.com/vivaui/viva/app"
	"github.com/vivaui/viva/ui"
	vapp "viff/internal/app"
	"viff/internal/domain"
	"viff/internal/platform"
)

// These acceptance probes encode expectations from external issue reports.
// The opt-in litheaudit tag groups the external-issue audit separately from
// ordinary regression tests. These probes now pass after the gap fixes.
func auditBounds(t *testing.T, f *uiFixture, id ui.ID) ui.Rect {
	t.Helper()
	for _, h := range f.s.Snapshot().Hits {
		if h.ID == id {
			return h.Bounds
		}
	}
	t.Fatalf("missing rendered control %v", id)
	return ui.Rect{}
}
func auditPaint(f *uiFixture) { f.win.Invalidate(); f.s.Settle(60*time.Millisecond, time.Second) }
func auditScroll(t *testing.T, f *uiFixture, r ui.Rect, dx, dy float32) {
	t.Helper()
	if err := f.s.Scroll(ui.ScrollEvent{X: r.X + r.Width/2, Y: r.Y + r.Height/2, DeltaX: dx, DeltaY: dy}); err != nil {
		t.Fatal(err)
	}
	f.s.Settle(40*time.Millisecond, time.Second)
}

func TestLitheIssuesBranchPrefixAndScroll(t *testing.T) {
	f := newUIFixture(t)
	for i := 0; i < 65; i++ {
		f.git("branch", fmt.Sprintf("audit-%03d-%s", i, strings.Repeat("long-branch-", 6)))
	}
	f.git("branch", "abcefg")
	f.git("branch", "abcefg-12345")
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.click("Branch: main")
	p := f.win.popups[0]
	found := map[string]bool{}
	for _, it := range p.items {
		found[it.title] = true
	}
	if !found["abcefg"] || !found["abcefg-12345"] {
		t.Fatal("#764 same-prefix branch missing")
	}
	r := p.r
	r.Y += 120
	r.Height -= 150
	if err := f.s.Move(r.X+40, r.Y+30); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		auditScroll(t, f, r, 0, 100)
		auditScroll(t, f, r, 0, -50)
	}
	if p.scroll.Y <= 0 {
		t.Error("#1621/#1277 popup does not scroll over branch rows")
	}
	f.snap("branch-prefix-and-scroll")
	for i := 0; i < 4; i++ {
		f.s.Keys("escape")
		f.click("Branch: main")
	}
	f.s.Keys("escape")
}

func TestLitheIssuesSingleUnstageRollbackSelectAll(t *testing.T) {
	f := newUIFixture(t)
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.click("README.md")
	f.s.Keys("space")
	f.settle()
	if got := f.git("diff", "--cached", "--name-only"); strings.TrimSpace(got) != "" {
		t.Fatalf("#530 single-file unstage failed: %s", got)
	}
	if !strings.Contains(f.git("diff", "--name-only"), "README.md") {
		t.Fatal("unstage lost worktree edit")
	}
	f.snap("single-file-unstaged")
	f.click("main.go")
	f.s.Keys("cmd+a")
	auditPaint(f)
	want := 0
	for _, r := range f.win.cm.rows {
		if r.kind == rowFile {
			want++
		}
	}
	if got := len(f.win.selectedEntries()); got != want {
		t.Errorf("#19 select all: got %d files want %d", got, want)
	}
	f.snap("select-all")
	f.click("main.go")
	f.win.openRollback()
	auditPaint(f)
	f.snap("rollback-confirmation")
	f.click("Rollback")
	f.settle()
	if got := f.git("diff", "--", "main.go"); got != "" {
		t.Errorf("#387 rollback did not restore file: %s", got)
	}
}

func TestLitheIssuesIgnoreSemantics(t *testing.T) {
	f := newUIFixture(t)
	f.write(".gitignore", "notes/\nREADME.md\n.build/\n")
	f.write(".build/nested/output.bin", "ignored\n")
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	for _, r := range f.win.cm.rows {
		if r.kind == rowFile && (strings.HasPrefix(r.entry.Path, "notes/") || strings.HasPrefix(r.entry.Path, ".build/")) {
			t.Errorf("#1445 ignored untracked path is visible: %s", r.entry.Path)
		}
	}
	tracked := false
	for _, r := range f.win.cm.rows {
		tracked = tracked || r.entry.Path == "README.md"
	}
	if !tracked {
		t.Error("#1445 already tracked README must remain visible despite ignore rule")
	}
	f.snap("ignored-untracked-tracked-visible")
}

func TestLitheIssuesDiffContextNavigationAndTabs(t *testing.T) {
	f := newUIFixture(t)
	var lines []string
	for i := 0; i < 120; i++ {
		lines = append(lines, fmt.Sprintf("line %03d %s", i, strings.Repeat("context ", 25)))
	}
	original := strings.Join(lines, "\n") + "\n"
	f.write("audit.txt", original)
	f.commit("Audit context", "2026-10-14T10:00:00")
	lines[40] = "changed first"
	lines[90] = "changed second"
	f.write("audit.txt", strings.Join(lines, "\n")+"\n")
	f.write("README.md", "second file changed\n")
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.click("audit.txt")
	f.settleTab()
	tab := f.win.ed.cur()
	if len(tab.oldDisp) < 120 || len(tab.newDisp) < 120 {
		t.Fatal("#557 unchanged context lost")
	}
	if strings.Join(tab.res.Old.Lines, "\n") == strings.Join(tab.res.New.Lines, "\n") {
		t.Error("#1123 both diff sides identical")
	}
	f.s.Keys("f7")
	auditPaint(f)
	if tab.cur != 1 {
		t.Errorf("#901 F7 expected second block, got %d", tab.cur)
	}
	f.s.Keys("shift+f7")
	auditPaint(f)
	if tab.cur != 0 {
		t.Errorf("#901 previous block got %d", tab.cur)
	}
	r := auditBounds(t, f, idDiffRight)
	auditScroll(t, f, r, 160, 0)
	if tab.x <= 0 {
		t.Error("#649 horizontal diff scrolling did not move")
	}
	f.snap("diff-context-and-navigation")
	f.s.DoubleClickLabel("audit.txt")
	f.settleTab()
	f.click("README.md")
	f.settleTab()
	if len(f.win.ed.tabs) < 2 {
		t.Errorf("#802/#1552 pinned diff replaced: %d tabs", len(f.win.ed.tabs))
	}
	f.snap("multiple-diff-tabs")
}

func TestLitheIssuesMultiCommitDetails(t *testing.T) {
	f := newUIFixture(t)
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.click("Log the listen address")
	f.settle()
	f.win.mods = ui.ModifierMeta
	f.win.clickCommit(1)
	f.win.mods = 0
	auditPaint(f)
	if len(f.win.selectedCommits()) != 2 {
		t.Fatal("setup did not select two commits")
	}
	if err := f.s.WaitFor(func(vivaapp.Snapshot) bool {
		return f.win.lg.multi.summary != nil && !f.win.lg.multi.loading
	}, 10*time.Second); err != nil {
		t.Fatal("multi-commit files did not load:", err)
	}
	sum := f.win.lg.multi.summary
	if sum.Err != nil {
		t.Fatal(sum.Err)
	}
	want := map[string]bool{"main.go": false, "internal/store/store.go": false}
	for i, file := range sum.Files {
		if _, ok := want[file.Path]; !ok {
			t.Errorf("unexpected aggregated path %q", file.Path)
		}
		want[file.Path] = true
		if len(file.Sources) != 1 {
			t.Errorf("unexpected sources for %s: %d", file.Path, len(file.Sources))
		}
		auditBounds(t, f, scMultiCommitRows.At(i))
	}
	for path, found := range want {
		if !found {
			t.Errorf("missing aggregated file %s", path)
		}
	}
	f.snap("multi-commit-details")

}

func TestLitheIssuesLongCommitMessage(t *testing.T) {
	f := newUIFixture(t)
	body := strings.Repeat("long paragraph content ", 350) + "TAIL_SENTINEL_771"
	f.git("commit", "--allow-empty", "-m", "Audit long message", "-m", body)
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.click("Audit long message")
	f.settle()
	if !strings.Contains(f.ws.Repo.Detail.Body, "TAIL_SENTINEL_771") {
		t.Fatal("Git message was truncated before UI")
	}
	f.win.lg.msgScroll.Y = 100000
	auditPaint(f)
	f.snap("long-message-bottom")
	if !f.s.HasText("TAIL_SENTINEL_771") {
		t.Error("#771 UNMET: long single-paragraph message tail cannot be displayed even at bottom")
	}
}

func TestLitheIssuesConsoleLongLine(t *testing.T) {
	f := newUIFixture(t)
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.ws.Console = []vapp.ConsoleEntry{{Time: time.Now(), Command: "git audit", Output: strings.Repeat("long-output-", 80) + "TAIL_SENTINEL_677"}}
	f.win.gitTab = 1
	auditPaint(f)
	r := auditBounds(t, f, idConsole)
	auditScroll(t, f, r, 1000, 0)
	f.snap("console-long-line")
	if f.win.con.scroll.X == 0 {
		t.Error("#677 UNMET: console has no horizontal scrolling for long unwrapped lines")
	}
}

func TestLitheIssuesAuthorFilterAndCherryPick(t *testing.T) {
	f := newUIFixture(t)
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.ws.SetLogFilter(vapp.LogFilter{Author: "no-such-author"})
	f.settle()
	if len(f.ws.Repo.Log.Commits) != 0 {
		t.Error("#1342 author filter includes unrelated commits")
	}
	f.ws.SetLogFilter(vapp.LogFilter{Branch: "fix/store-locking", Author: "Ada Lovelace"})
	f.settle()
	f.win.lg.sel = map[string]bool{f.ws.Repo.Log.Commits[0].Hash: true}
	auditPaint(f)
	it, ok := hasItem(f.win.commitMenu(), "Cherry-Pick")
	if !ok {
		for _, x := range f.win.commitMenu() {
			if strings.Contains(x.title, "Cherry") {
				it = x
				ok = true
			}
		}
	}
	if !ok || it.disabled {
		t.Errorf("#1662 first row of another branch must allow cherry-pick: %+v", it)
	}
	for _, c := range f.ws.Repo.Log.Commits {
		if c.Author != "Ada Lovelace" {
			t.Error("wrong author")
		}
	}
	f.snap("author-filter-other-branch")
}

func TestLitheIssuesRepeatedLogScroll(t *testing.T) {
	f := newUIFixture(t)
	for i := 0; i < 55; i++ {
		f.git("commit", "--allow-empty", "-m", fmt.Sprintf("Scroll audit %02d", i))
	}
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	r := auditBounds(t, f, idLogTable)
	for i := 0; i < 10; i++ {
		before := f.win.lg.scroll.Y
		auditScroll(t, f, r, 0, 70)
		if f.win.lg.scroll.Y <= before {
			t.Fatalf("#152 log stopped scrolling on cycle %d", i)
		}
		auditScroll(t, f, r, 0, -50)
	}
	f.snap("repeated-log-scroll")
}

func TestLitheIssuesExternalGitAutoRefresh(t *testing.T) {
	f := newUIFixture(t)
	f.ws.Watch = func(repo domain.Repo, changed func(domain.RepoChange)) (vapp.Watcher, error) {
		return platform.Watch(repo, changed, platform.WatchOptions{})
	}
	f.start(1280, 800)
	f.win.openRepo(f.dir)
	f.settle()
	f.s.Settle(350*time.Millisecond, time.Second)
	f.git("commit", "--allow-empty", "-m", "External audit commit")
	want := strings.TrimSpace(f.git("rev-parse", "HEAD"))
	f.git("branch", "external-audit-branch")
	if err := f.s.WaitFor(func(vivaapp.Snapshot) bool {
		rs := f.ws.Repo
		if len(rs.Log.Commits) == 0 || rs.Log.Commits[0].Hash != want {
			return false
		}
		for _, r := range rs.RefsOf(domain.RefLocal) {
			if r.Short == "external-audit-branch" {
				return true
			}
		}
		return false
	}, 10*time.Second); err != nil {
		t.Fatalf("#22 native watcher failed to refresh external commit/branch: %v", err)
	}
	f.settle()
	f.snap("external-git-auto-refreshed")
}
