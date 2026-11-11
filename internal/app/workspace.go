// Package app holds Viff's use cases: opening repositories, querying their
// state, running Git commands as tasks, and turning file-system changes into
// refreshes. Git stays the source of truth: every snapshot here is a cache
// that is rebuilt by asking Git again.
//
// Threading: all state is read and changed on the UI thread. Background work
// runs in goroutines that only call Git and then post a closure that applies
// the result on the UI thread (Drain). A result carries the generation it was
// started for; one that arrives after the repository or the selection
// changed is dropped instead of overwriting the newer view.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"viff/internal/domain"
	"viff/internal/gitcli"
	"viff/internal/i18n"
	"viff/internal/observability"
	"viff/internal/vcs"
)

// Notification is a balloon shown to the user.
type Notification struct {
	ID      uint64
	Kind    NoticeKind
	Title   string
	Body    string
	Time    time.Time
	Actions []NoticeAction
	Sticky  bool
}

// NoticeKind is the severity of a notification.
type NoticeKind uint8

const (
	NoticeInfo NoticeKind = iota
	NoticeSuccess
	NoticeWarning
	NoticeError
)

// NoticeAction is a link in a notification.
type NoticeAction struct {
	Title string
	Run   func()
}

// ConsoleEntry is one Git command shown in the Console tab.
type ConsoleEntry struct {
	Time    time.Time
	Command string
	Output  string
	Failed  bool
	Took    time.Duration
}

// Task is a running command shown in the status bar.
type Task struct {
	ID       uint64
	Title    string
	Progress string
	Started  time.Time
	cancel   context.CancelFunc
	// cancelled reports that the user cancelled the task, so its end event can
	// distinguish a cancel from a normal finish.
	cancelled bool
	Write     bool
}

// Workspace is the application state for one window.
type Workspace struct {
	Git      Git
	Settings *Settings

	// Repo is the open repository, nil on the welcome screen.
	Repo *RepoState
	// lfsScans keeps a canceled scan reserved until its process has exited,
	// including when the same worktree was closed and immediately reopened.
	lfsScans map[string]*lfsScan

	// Caps are the optional features of the active backend, derived from the
	// instance by NewWorkspace. The UI hides what the backend cannot do instead
	// of showing an entry that fails later, which is how rebase and stash stay
	// visible for Git and absent for Subversion.
	Caps Capabilities

	// ConsoleTool is the executable name the console and the event log show for
	// the active backend ("git" or "svn"). It is set with the backend, so a
	// command is never labelled with the other tool's name.
	ConsoleTool string

	// Repositories is the explicit multi-repository list, in the order the user
	// added them. Only the active repository (Repo) runs Git commands and has a
	// file watcher; the other rows show their last known status and are
	// refreshed by RefreshRepositories.
	Repositories []RepositorySummary
	// ActiveRoot is the canonical root of the active repository, "" when none
	// is open. It is persisted with the list, so a restart can restore the
	// previous selection.
	ActiveRoot string
	// WorkspaceWarning reports a workspace file that could not be used as is
	// (unreadable, or written by a newer Viff and therefore read-only).
	WorkspaceWarning string
	// ChangeLists is the per-repository change-list bookkeeping. It is created
	// lazily by the change-list backend (changelists.go); a workspace that never
	// shows change lists keeps it nil.
	ChangeLists *ChangeListManager
	// Prompts is the per-repository choice of AI commit-message rules plus the
	// rules the user wrote. Like ChangeLists it is created lazily (prompts.go),
	// so a workspace that never generates a message keeps it nil.
	Prompts *PromptManager
	// changeListSyncWarned keeps a failing rename sync from notifying on every
	// status refresh; the next success clears it.
	changeListSyncWarned bool
	// workspaceSaveWarned keeps a failing workspace write from notifying on
	// every list change; the next success clears it.
	workspaceSaveWarned bool

	// remotePollEvery and remoteMetaEvery are test seams for the SSH watcher.
	// Zero means the production defaults in remote_watch.go.
	remotePollEvery time.Duration
	remoteMetaEvery int

	Notifications []Notification
	Console       []ConsoleEntry
	Tasks         []*Task

	// Update is the in-app update flow rendered by the update dialog. The UI
	// only reads it; CheckForUpdates, DownloadUpdate, and InstallUpdate move
	// it forward (update.go). Background work applies its result through post.
	Update UpdateState
	// updates is the private state behind Update; see update.go.
	updates updateMachine

	// obs records structured events. It is observability.Nop{} until the
	// composition root installs a logger, so call sites never check for nil
	// and a workspace without logging behaves exactly like before.
	obs observability.Recorder
	// inc starts and stops diagnostic recordings; Nop when logging is off.
	inc observability.IncidentController
	// logLevel is the configured level name ("info" or "debug"). It is kept
	// so a recorder installed later still receives the configured level.
	logLevel string

	// repos is the private state behind Repositories; see repositories.go.
	repos repositoriesState
	// undos is the session undo history, newest last; undoing marks a running
	// Undo task. Both are UI-thread state and are cleared when the repository
	// changes (see undo.go).
	undos   []*undoEntry
	undoing bool
	// searchSeq and searchCancel belong to SearchContent: a query that is no
	// longer the newest one is dropped after it returns.
	searchSeq    uint64
	searchCancel context.CancelFunc

	// nextGen is the repository generation counter. It only ever grows: a
	// generation is never reused after a repository is closed or removed, so a
	// stale view can never match a newer repository.
	nextGen uint64

	invalidate func()
	mu         sync.Mutex
	pending    []func()
	nextID     uint64
	watcher    Watcher
	// backends are the adapters this workspace may use, registered by the
	// composition root. A single entry means one version control system; the
	// field is written before any command runs and read-only afterwards.
	backends []probeTarget
	// Watch starts file-system watching for a repository; set by the
	// composition root so platform code stays out of this package.
	Watch func(repo domain.Repo, changed func(domain.RepoChange)) (Watcher, error)
	// ConfirmCheckout asks the UI whether to stash local changes and retry a
	// checkout Git blocked. It runs on the UI thread and returns true when the
	// user accepted; nil reports the failure instead of offering the choice.
	// Changes are never stashed without this confirmation.
	ConfirmCheckout func(CheckoutBlocked) bool
	// Now is the clock (replaceable in tests).
	Now func() time.Time
	// startedAt anchors startup duration measurement (app.ready).
	startedAt time.Time
	// readyLogged keeps the app.ready event to the first paint.
	readyLogged bool
}

// Watcher stops a file-system watch.
type Watcher interface{ Close() }

// NewWorkspace creates the state. invalidate is called from any goroutine
// when results are waiting; the UI then calls Drain on its thread.
//
// Logging is off until SetRecorder installs a recorder: the default Nop
// recorder keeps every existing test and a build without a config directory
// working unchanged.
func NewWorkspace(git Git, settings *Settings, invalidate func()) *Workspace {
	w := &Workspace{Git: git, Settings: settings, invalidate: invalidate, Now: time.Now, obs: observability.Nop{}, inc: observability.Nop{}, startedAt: time.Now()}
	w.Caps = CapabilitiesOf(git)
	w.SetLogLevel("")
	w.initUpdateState()
	if settings != nil {
		w.SetLogLevel(settings.LogLevel)
	}
	git.SetCommandLog(w.logCommand)
	w.loadRepositories()
	w.ApplyGitSettings()
	return w
}

// RecordReady reports that the first frame has started painting. The UI calls it
// once; the event carries the elapsed time since workspace creation so slow
// starts can be measured from the log alone. Later calls do nothing.
func (w *Workspace) RecordReady() {
	if w.readyLogged {
		return
	}
	w.readyLogged = true
	d := time.Since(w.startedAt)
	w.record(observability.Event{
		Event: observability.EventAppReady, Level: observability.LevelInfo,
		Dur: d, Fields: map[string]any{"startup_ms": d.Milliseconds()},
	})
}

// now is the workspace clock, falling back to the real time when a Workspace
// was built as a literal without NewWorkspace (tests).
func (w *Workspace) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// post queues fn to run on the UI thread.
func (w *Workspace) post(fn func()) {
	w.mu.Lock()
	w.pending = append(w.pending, fn)
	w.mu.Unlock()
	if w.invalidate != nil {
		w.invalidate()
	}
}

// Drain applies queued results. It returns whether anything changed.
func (w *Workspace) Drain() bool {
	w.mu.Lock()
	fns := w.pending
	w.pending = nil
	w.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return len(fns) > 0
}

// Pending reports queued results (for tests that wait for background work).
func (w *Workspace) Pending() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) > 0
}

func (w *Workspace) id() uint64 {
	w.nextID++
	return w.nextID
}

const consoleLimit = 400

// consoleOutputLimit bounds one Console entry's output. The Console tab renders
// an entry as one row, so an unbounded stderr would cost memory and painting
// time for no diagnostic value; 8 KiB is far more than any Git message.
const consoleOutputLimit = 8 << 10

// failStderrLimit bounds the stderr summary attached to a git.fail event. The
// logger redacts the value on its way to disk, so the application passes the
// raw Git output (see the observability contract).
const failStderrLimit = 2 << 10

// slowRead is the duration after which a successful read command is recorded
// as a git.end event even though reads are otherwise not worth a log line:
// slow reads are what "Viff feels slow" reports are made of.
const slowRead = 500 * time.Millisecond

// quietReads are the commands whose successful result is console noise: they
// run on every refresh. A failing read is always shown. The list mirrors the
// old inline switch and adds the read commands it missed, so long-running
// reads cannot push failure context out of the Console tab.
var quietReads = map[string]bool{
	"status": true, "log": true, "for-each-ref": true, "cat-file": true,
	"show": true, "diff-tree": true, "rev-parse": true, "ls-files": true,
	"reflog": true, "remote": true, "config": true, "worktree": true,
	"merge-base": true, "diff": true, "diff-files": true, "rev-list": true,
	"ls-tree": true, "blame": true, "ls-remote": true, "show-ref": true,
	"symbolic-ref": true, "describe": true, "grep": true, "check-ignore": true,
	"check-ref-format": true, "name-rev": true, "stash": true,
	// The capability probe is a read; its failure is still shown.
	"lfs": true,
	// --version is not a subcommand, but the startup probe goes through
	// Runner.Log and must not appear as Console noise before any repository
	// is open.
	"--version": true,
}

// writeCommands are the Git subcommands that change the repository, the index
// or the working tree. Ambiguous families (branch, tag, stash, remote, config,
// worktree, submodule) are decided per invocation in commandWrites.
var writeCommands = map[string]bool{
	"add": true, "am": true, "apply": true, "checkout": true,
	"cherry-pick": true, "clean": true, "clone": true, "commit": true,
	"fetch": true, "filter-branch": true, "gc": true, "init": true,
	"merge": true, "mv": true, "pull": true, "push": true, "rebase": true,
	"replace": true, "reset": true, "restore": true, "revert": true, "rm": true,
	"switch": true, "update-index": true, "update-ref": true,
}

// commandWrites reports whether args runs a command that changes repository
// state. It is a heuristic used only to label events and Console entries; a
// wrong answer costs a log field, never correctness.
func commandWrites(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "stash":
		if len(args) < 2 {
			return false
		}
		switch args[1] {
		case "apply", "pop", "drop", "push", "save", "clear", "branch", "create", "store":
			return true
		}
		return false
	case "branch":
		// `git branch` lists; creating, renaming, deleting and setting the
		// upstream take arguments.
		return len(args) > 1 && !hasFlagPrefix(args[1:], "--list", "-l", "--show-current", "--merged", "--no-merged", "--contains")
	case "tag":
		return len(args) > 1 && !hasFlagPrefix(args[1:], "-l", "--list", "-n", "--contains", "--merged", "--no-merged")
	case "worktree":
		return len(args) < 2 || args[1] != "list"
	case "remote":
		if len(args) < 2 {
			return false
		}
		switch args[1] {
		case "add", "remove", "rm", "rename", "set-url", "set-head", "set-branches", "prune", "update":
			return true
		}
		return false
	case "config":
		for _, a := range args[1:] {
			switch a {
			case "--get", "--get-all", "--get-regexp", "--list", "-l", "--show-origin", "--show-scope":
				return false
			}
		}
		return len(args) > 1
	case "submodule":
		return len(args) < 2 || args[1] != "status"
	}
	return writeCommands[args[0]]
}

// hasFlagPrefix reports whether args starts with one of flags.
func hasFlagPrefix(args []string, flags ...string) bool {
	if len(args) == 0 {
		return false
	}
	for _, f := range flags {
		if args[0] == f || strings.HasPrefix(args[0], f+"=") {
			return true
		}
	}
	return false
}

// logCommand records commands in the console. It runs on worker goroutines.
//
// It has two independent jobs: keeping the Console tab readable (silent
// successful reads, failures always shown) and reporting the same command to
// the structured event log (every failure, every successful write, every slow
// read). The default Nop recorder makes the event half a no-op.
func (w *Workspace) logCommand(dir string, args []string, took time.Duration, err error) {
	if len(args) == 0 {
		return
	}
	op := args[0]
	// The stats event reports the outcome and code of the whole listing.
	// Do not copy a partial list or a path-bearing error to logs or Console.
	if op == "lfs" && len(args) > 1 && args[1] == "ls-files" {
		return
	}
	write := commandWrites(args)
	if err != nil {
		w.record(observability.Event{
			Event:  observability.EventGitFail,
			Level:  observability.LevelError,
			Repo:   dir,
			Op:     op,
			Code:   gitcli.CodeOf(err),
			Dur:    took,
			Fields: gitFailFields(err, write),
		})
	} else if write || took > slowRead {
		w.record(observability.Event{
			Event:  observability.EventGitEnd,
			Level:  observability.LevelInfo,
			Repo:   dir,
			Op:     op,
			Dur:    took,
			Fields: map[string]any{"write": write, "exit": 0},
		})
	}
	if err == nil && quietRead(args) {
		return // reads are not console noise unless they fail
	}
	tool := w.ConsoleTool
	if tool == "" {
		tool = toolName(w.Git)
	}
	entry := ConsoleEntry{Time: w.now(), Command: tool + " " + strings.Join(quoteArgs(args), " "), Took: took}
	if err != nil {
		entry.Failed = true
		var ge *gitcli.Error
		if errors.As(err, &ge) {
			entry.Output = truncateBytes(strings.TrimSpace(ge.Stderr+"\n"+ge.Stdout), consoleOutputLimit)
		} else {
			entry.Output = truncateBytes(err.Error(), consoleOutputLimit)
		}
	}
	w.post(func() {
		w.Console = append(w.Console, entry)
		if len(w.Console) > consoleLimit {
			w.Console = append([]ConsoleEntry(nil), w.Console[len(w.Console)-consoleLimit:]...)
		}
	})
}

// quietRead reports whether a successful invocation of args is Console noise.
func quietRead(args []string) bool {
	if len(args) == 0 || !quietReads[args[0]] {
		return false
	}
	// `git stash list` is a read; the other stash subcommands change state and
	// stay visible exactly as before.
	return args[0] != "stash" || len(args) < 2 || args[1] == "list"
}

// gitFailFields is the small, log-safe payload of a git.fail event. The raw
// stderr is capped here; the logger redacts it before it reaches a file.
func gitFailFields(err error, write bool) map[string]any {
	fields := map[string]any{
		"exit":  -1,
		"kind":  gitcli.KindOf(err).String(),
		"code":  gitcli.CodeOf(err),
		"write": write,
	}
	var ge *gitcli.Error
	if errors.As(err, &ge) {
		if ge.ExitCode != 0 {
			fields["exit"] = ge.ExitCode
		}
		stderr := strings.TrimSpace(ge.Stderr)
		if stderr == "" {
			stderr = strings.TrimSpace(ge.Stdout)
		}
		if stderr == "" {
			stderr = ge.Message()
		}
		if stderr != "" {
			fields["stderr"] = truncateBytes(stderr, failStderrLimit)
		}
		return fields
	}
	if msg := strings.TrimSpace(err.Error()); msg != "" {
		fields["stderr"] = truncateBytes(msg, failStderrLimit)
	}
	return fields
}

// truncateBytes shortens s to at most limit bytes, keeping the text valid
// UTF-8 and marking the cut with an ellipsis.
func truncateBytes(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	if cut > len("…") {
		cut -= len("…")
	}
	s = s[:cut]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n\"'") {
			out[i] = fmt.Sprintf("%q", a)
		} else {
			out[i] = a
		}
	}
	return out
}

// Notify adds a notification.
func (w *Workspace) Notify(kind NoticeKind, title, body string, actions ...NoticeAction) {
	w.notify(kind, title, body, "", actions...)
}

// notify is Notify with the stable error code, "" when there is none, so the
// event log can group notifications by cause.
func (w *Workspace) notify(kind NoticeKind, title, body, code string, actions ...NoticeAction) {
	n := Notification{ID: w.id(), Kind: kind, Title: title, Body: body, Time: w.now(), Actions: actions}
	w.Notifications = append(w.Notifications, n)
	if len(w.Notifications) > 50 {
		w.Notifications = w.Notifications[len(w.Notifications)-50:]
	}
	fields := map[string]any{"level": noticeLevelName(kind), "title": title}
	if code != "" {
		fields["code"] = code
	}
	w.record(observability.Event{
		Event:  observability.EventUINotice,
		Level:  noticeEventLevel(kind),
		Repo:   w.repoRoot(),
		Op:     "notify",
		Code:   code,
		Fields: fields,
	})
}

// tr translates an application message into the language the settings name.
// English is the default, so an empty language, and a workspace built without
// settings, keep the original text.
func (w *Workspace) tr(key i18n.Key, args ...any) string {
	return w.translator().T(key, args...)
}

// trPlural translates a counted message: the singular form for one, the plural
// one otherwise. Chinese spells both the same way.
func (w *Workspace) trPlural(n int, one, many i18n.Key, args ...any) string {
	return w.translator().Plural(n, one, many, args...)
}

// translator speaks the language the settings name. It reads the settings on
// every call, so a change applies to the next message instead of the next
// start, and it holds no state of its own.
func (w *Workspace) translator() *i18n.Translator {
	lang := ""
	if w.Settings != nil {
		lang = w.Settings.Language
	}
	return i18n.New(i18n.Parse(lang))
}

// noticeLevelName is the stable name of a notice kind in event fields.
func noticeLevelName(kind NoticeKind) string {
	switch kind {
	case NoticeSuccess:
		return "success"
	case NoticeWarning:
		return "warning"
	case NoticeError:
		return "error"
	}
	return "info"
}

// noticeEventLevel maps a notice to the event level: only errors and warnings
// are worth keeping at info level, everything else is context.
func noticeEventLevel(kind NoticeKind) observability.Level {
	switch kind {
	case NoticeWarning, NoticeError:
		return observability.LevelWarn
	}
	return observability.LevelInfo
}

// Dismiss removes a notification.
func (w *Workspace) Dismiss(id uint64) {
	for i, n := range w.Notifications {
		if n.ID == id {
			w.Notifications = append(w.Notifications[:i:i], w.Notifications[i+1:]...)
			return
		}
	}
}

// warnChangeListSync reports a failed change-list rename sync once per failure
// streak. SyncChangeListRenames runs after every successful status refresh, so
// a persistent failure would otherwise repeat the warning on every refresh.
func (w *Workspace) warnChangeListSync(err error) {
	if err == nil {
		w.changeListSyncWarned = false
		return
	}
	if w.changeListSyncWarned {
		return
	}
	w.changeListSyncWarned = true
	w.Notify(NoticeWarning, w.tr(i18n.AppNoticeChangeLists), err.Error())
}

// NotifyError shows a Git failure with its message. The notification carries
// the stable error code in the event log, so a user can quote it in a report.
func (w *Workspace) NotifyError(title string, err error) {
	if err == nil {
		return
	}
	if gitcli.KindOf(err) == gitcli.ErrCancelled {
		w.notify(NoticeInfo, title, w.tr(i18n.AppNoticeCancelled), gitcli.CodeOf(err))
		return
	}
	w.notify(NoticeError, title, w.ErrorText(err), gitcli.CodeOf(err))
}

// ReadOnly reports whether the active repository is opened over SSH and
// therefore accepts no writes in this version. The interface uses it to hide
// write actions instead of showing them and failing afterwards.
func (w *Workspace) ReadOnly() bool {
	return w.Repo != nil && w.Repo.Repo.IsSSH()
}

// ErrorText is the message of an error suitable for the UI, with a hint for
// failures the user can act on.
//
// The hints name the backend because the advice differs: Git goes through the
// credential helper and the SSH agent, Subversion through its own auth cache.
// Telling a Subversion user to configure a Git credential helper would send
// them looking in the wrong place, so the tool recorded on the error decides
// which hint is right.
func (w *Workspace) ErrorText(err error) string {
	svn := errorFromSVN(err)
	var ge *gitcli.Error
	msg := err.Error()
	if errors.As(err, &ge) {
		msg = ge.Message()
	}
	msg = strings.TrimPrefix(msg, "fatal: ")
	msg = strings.TrimPrefix(msg, "error: ")
	switch gitcli.KindOf(err) {
	case gitcli.ErrAuth:
		if svn {
			msg += "\n\n" + w.tr(i18n.AppErrorAuthHintSVN)
		} else {
			msg += "\n\n" + w.tr(i18n.AppErrorAuthHintGit)
		}
	case gitcli.ErrLocked:
		if svn {
			msg += "\n\n" + w.tr(i18n.AppErrorLockedHintSVN)
		} else {
			msg += "\n\n" + w.tr(i18n.AppErrorLockedHintGit)
		}
	case gitcli.ErrNoGit:
		if svn {
			msg = w.tr(i18n.AppErrorToolMissingSVN)
		} else {
			msg = w.tr(i18n.AppErrorToolMissingGit)
		}
	case gitcli.ErrNoSSH:
		msg = w.tr(i18n.AppErrorSSHMissing)
	case gitcli.ErrSSHConnect:
		msg += "\n\n" + w.tr(i18n.AppErrorSSHConnect)
	case gitcli.ErrSSHAuth:
		msg += "\n\n" + w.tr(i18n.AppErrorSSHAuth)
	case gitcli.ErrSSHHostKey:
		msg += "\n\n" + w.tr(i18n.AppErrorSSHHostKey)
	case gitcli.ErrSSHGitMissing:
		msg = w.tr(i18n.AppErrorSSHGitMissing)
	case gitcli.ErrReadOnlyRemote:
		msg = w.tr(i18n.AppErrorReadOnlyRemote)
	}
	if errors.Is(err, ErrUnsupported) {
		msg = w.tr(i18n.AppErrorUnsupported)
	}
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	return msg
}

// errorFromSVN reports whether the failure came from the Subversion adapter.
func errorFromSVN(err error) bool {
	var ve *vcs.Error
	return errors.As(err, &ve) && ve.Tool == "svn"
}

// ErrorText is the backend-agnostic form used where no workspace is at hand.
// It keeps the Git wording, which is what a caller outside this package had
// before; anything user-facing inside the application uses the method above so
// the hint matches the open repository.
func ErrorText(err error) string {
	var ge *gitcli.Error
	msg := err.Error()
	if errors.As(err, &ge) {
		msg = ge.Message()
	}
	msg = strings.TrimPrefix(msg, "fatal: ")
	msg = strings.TrimPrefix(msg, "error: ")
	switch gitcli.KindOf(err) {
	case gitcli.ErrAuth:
		msg += "\n\nAuthentication failed. Viff uses your Git credential helper and SSH agent; configure them in a terminal and try again."
	case gitcli.ErrLocked:
		msg += "\n\nAnother Git process is using this repository. Wait for it to finish; remove the .lock file only if no Git process is running."
	case gitcli.ErrNoGit:
		msg = "Git was not found. Install Git or set its path in Settings."
	case gitcli.ErrNoWSL:
		msg = "WSL was not found. Open WSL repositories by installing WSL (wsl --install in a terminal) and trying again."
	case gitcli.ErrNoDistro:
		msg = "The WSL distribution named in the path does not exist. Check the spelling — a WSL repository path looks like \\\\wsl.localhost\\Ubuntu\\home\\you\\repo."
	case gitcli.ErrNoWSLGit:
		msg = "Git is not installed inside the WSL distribution. Install it there (for example: sudo apt install git) and try again."
	case gitcli.ErrNoSSH:
		msg = "SSH was not found. Install an OpenSSH client and try again."
	case gitcli.ErrSSHConnect:
		msg += "\n\nViff could not reach the server. Check the host, port and network."
	case gitcli.ErrSSHAuth:
		msg += "\n\nSSH authentication failed. Load a key into ssh-agent or configure IdentityFile in ~/.ssh/config."
	case gitcli.ErrSSHHostKey:
		msg += "\n\nThe host key is not trusted yet. Run `ssh <host>` once in a terminal, then retry."
	case gitcli.ErrSSHGitMissing:
		msg = "Git is not installed on the server, or the non-interactive SSH PATH cannot find it."
	case gitcli.ErrReadOnlyRemote:
		msg = "This remote repository is read-only in Viff. Change it on the server or in another terminal."
	}
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	return msg
}

// startTask registers a task; the returned done function removes it. Both the
// start and the end are recorded as events: a task that never ends is exactly
// what a "frozen while pushing" report looks like.
func (w *Workspace) startTask(title string, write bool) (*Task, context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{ID: w.id(), Title: title, Started: w.now(), cancel: cancel, Write: write}
	w.Tasks = append(w.Tasks, t)
	key := taskTitleKey(title)
	w.record(observability.Event{
		Event:  observability.EventTaskStart,
		Level:  observability.LevelInfo,
		Repo:   w.repoRoot(),
		Op:     key,
		Fields: map[string]any{"task": t.ID, "title_key": key, "write": write},
	})
	return t, ctx, func() {
		cancel()
		for i, x := range w.Tasks {
			if x == t {
				w.Tasks = append(w.Tasks[:i:i], w.Tasks[i+1:]...)
				break
			}
		}
		status := "done"
		if t.cancelled {
			status = "cancelled"
		}
		w.record(observability.Event{
			Event:  observability.EventTaskEnd,
			Level:  observability.LevelInfo,
			Repo:   w.repoRoot(),
			Op:     key,
			Dur:    w.now().Sub(t.Started),
			Fields: map[string]any{"task": t.ID, "status": status},
		})
	}
}

// taskTitleKey is the coarse action name of a task title for event fields. The
// full title may contain repository or branch names, so only the first word
// ("push", "commit", "opening") is recorded.
func taskTitleKey(title string) string {
	fields := strings.Fields(title)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(fields[0], ":"))
}

// CancelTask cancels a running task. Cancelling a write stops the process;
// it does not undo what Git already did, and the state is queried again.
func (w *Workspace) CancelTask(id uint64) {
	for _, t := range w.Tasks {
		if t.ID == id && t.cancel != nil {
			if !t.cancelled {
				t.cancelled = true
				w.record(observability.Event{
					Event:  observability.EventTaskCancel,
					Level:  observability.LevelInfo,
					Repo:   w.repoRoot(),
					Op:     taskTitleKey(t.Title),
					Fields: map[string]any{"task": t.ID, "elapsed_ms": w.now().Sub(t.Started).Milliseconds()},
				})
			}
			t.cancel()
		}
	}
}

// Busy reports a running write task.
func (w *Workspace) Busy() bool {
	for _, t := range w.Tasks {
		if t.Write {
			return true
		}
	}
	return false
}

// SetProgress updates a task's progress text from a worker goroutine.
func (w *Workspace) setProgress(t *Task, line string) {
	w.post(func() { t.Progress = line })
}
