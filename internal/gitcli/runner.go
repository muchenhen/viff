// Package gitcli runs the official Git CLI and parses its machine-readable
// output. It builds argument arrays (never shell strings), consumes stderr
// with a bound, and classifies failures. It knows nothing about the UI.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"viff/internal/process"
	"viff/internal/sshpath"
	"viff/internal/vcs"
	"viff/internal/wslpath"
)

// stderrLimit bounds the stderr kept for an error message.
const stderrLimit = 64 << 10

// The error model lives in internal/vcs so a second backend reports failures
// through the same kinds and the same stable codes. These aliases keep the
// existing gitcli call sites and tests compiling unchanged; new code should
// use vcs directly.
type (
	// ErrorKind classifies a failure so the UI can offer the right action.
	ErrorKind = vcs.ErrorKind
	// Error is a failed invocation with the context needed to diagnose it.
	Error = vcs.Error
)

const (
	ErrOther                 = vcs.ErrOther
	ErrCancelled             = vcs.ErrCancelled
	ErrAuth                  = vcs.ErrAuth
	ErrLocked                = vcs.ErrLocked
	ErrNotRepository         = vcs.ErrNotRepository
	ErrConflict              = vcs.ErrConflict
	ErrNotFound              = vcs.ErrNotFound
	ErrRejected              = vcs.ErrRejected
	ErrUntrackedOverwrite    = vcs.ErrUntrackedOverwrite
	ErrLocalChangesOverwrite = vcs.ErrLocalChangesOverwrite
	ErrNotMerged             = vcs.ErrNotMerged
	ErrNothingToCommit       = vcs.ErrNothingToCommit
	ErrHookFailed            = vcs.ErrHookFailed
	ErrNoGit                 = vcs.ErrNoGit
	// ErrNoWSL, ErrNoDistro and ErrNoWSLGit are the three ways a WSL-backed
	// invocation fails before Git starts (wsl.go, wsl_windows.go).
	ErrNoWSL    = vcs.ErrNoWSL
	ErrNoDistro = vcs.ErrNoDistro
	ErrNoWSLGit = vcs.ErrNoWSLGit
	// ErrNoSSH, ErrSSHConnect, ErrSSHAuth, ErrSSHHostKey and ErrSSHGitMissing
	// are the ways a remote repository fails before its Git runs; the fixes
	// differ, so they keep separate codes. ErrReadOnlyRemote is the refusal of
	// every write on a repository opened for observation.
	ErrNoSSH          = vcs.ErrNoSSH
	ErrSSHConnect     = vcs.ErrSSHConnect
	ErrSSHAuth        = vcs.ErrSSHAuth
	ErrSSHHostKey     = vcs.ErrSSHHostKey
	ErrSSHGitMissing  = vcs.ErrSSHGitMissing
	ErrReadOnlyRemote = vcs.ErrReadOnlyRemote
)

// KindOf returns the error kind of err, ErrOther for foreign errors.
func KindOf(err error) ErrorKind { return vcs.KindOf(err) }

// CodeOf returns the stable error code for err, "" for nil.
func CodeOf(err error) string {
	if errors.Is(err, ErrLFSReapUnconfirmed) {
		return "lfs.cleanup_unconfirmed"
	}
	return vcs.CodeOf(err)
}

// firstArg is re-exported for the message format kept below.
func firstArg(args []string) string { return vcs.FirstArg(args) }

func classify(stderr string, exit int, ctxErr error) ErrorKind {
	if ctxErr != nil {
		return ErrCancelled
	}
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "authentication failed"), strings.Contains(s, "could not read username"),
		strings.Contains(s, "permission denied (publickey"), strings.Contains(s, "terminal prompts disabled"),
		strings.Contains(s, "invalid username or password"), strings.Contains(s, "returned error: 403"), strings.Contains(s, "returned error: 401"):
		return ErrAuth
	case strings.Contains(s, "index.lock"), strings.Contains(s, "unable to create") && strings.Contains(s, ".lock"):
		return ErrLocked
	case strings.Contains(s, "not a git repository"):
		return ErrNotRepository
	case strings.Contains(s, "untracked working tree files would be overwritten"):
		return ErrUntrackedOverwrite
	case strings.Contains(s, "your local changes to the following files would be overwritten"):
		return ErrLocalChangesOverwrite
	case strings.Contains(s, "conflict"), strings.Contains(s, "could not apply"), strings.Contains(s, "fix conflicts"):
		return ErrConflict
	case strings.Contains(s, "[rejected]"), strings.Contains(s, "non-fast-forward"), strings.Contains(s, "fetch first"), strings.Contains(s, "stale info"):
		return ErrRejected
	case strings.Contains(s, "is not fully merged"):
		return ErrNotMerged
	case strings.Contains(s, "nothing to commit"), strings.Contains(s, "nothing added to commit"):
		return ErrNothingToCommit
	case strings.Contains(s, "hook") && (strings.Contains(s, "failed") || strings.Contains(s, "exit")):
		return ErrHookFailed
	case strings.Contains(s, "does not exist"), strings.Contains(s, "unknown revision"), strings.Contains(s, "bad revision"), strings.Contains(s, "did not match any"):
		return ErrNotFound
	}
	return ErrOther
}

// Config is the configuration Viff passes to its own invocations. It is shared
// with every backend and lives in internal/vcs, so the settings page can push
// one value regardless of which one is active.
type Config = vcs.Config

// configArgs renders the configuration as -c arguments. Git applies them after
// the system and user configuration and before the command, so they win
// without editing the repository or the user's config.
func configArgs(c Config) []string {
	var out []string
	add := func(key, value string) {
		if value != "" {
			out = append(out, "-c", key+"="+value)
		}
	}
	add("gpg.program", c.GPGProgram)
	add("user.signingkey", c.SigningKey)
	add("gpg.format", c.SignFormat)
	add("credential.helper", c.CredentialHelper)
	if c.CommitSigning {
		out = append(out, "-c", "commit.gpgsign=true")
	}
	return out
}

// Runner executes git. The zero value uses "git" from PATH.
type Runner struct {
	// Path is the git executable; "" means look it up on PATH.
	Path string
	// Env is added to the inherited environment.
	Env []string
	// Log, when set, receives one line per finished command.
	Log func(dir string, args []string, took time.Duration, err error)
	// SSH is the transport for repositories opened over ssh://. It is nil in
	// production, where the system ssh runs the command; tests replace it with
	// a local transport so the rest of the stack runs on every platform.
	SSH SSHTransport
	// LFSCommand runs one git-lfs subcommand; args are those after "lfs". It
	// is nil in production, where the real executable runs through Path, and
	// tests set it to simulate a missing or installed git-lfs.
	LFSCommand func(ctx context.Context, dir string, args ...string) ([]byte, error)

	lfsMu        sync.Mutex
	lfsProbeKey  string
	lfsProbed    bool
	lfsInstalled bool
	lfsVersion   string

	resolveMu sync.Mutex
	pathReady bool
	resolved  string
	findErr   error

	cfgMu sync.Mutex
	cfg   Config

	// WSL state: which distros were probed to have a working git, and the
	// resolved wsl.exe. Guarded by wslMu; wslProbe and wslExeOverride are the
	// test seams (like LFSCommand).
	wslMu          sync.Mutex
	wslReady       map[string]bool
	wslGitPath     map[string]string
	wslExe         string
	wslExeKnown    bool
	wslExeErr      error
	wslProbe       func(ctx context.Context, distro string) error
	wslExeOverride string
}

// SetConfig replaces the configuration used by commands started afterwards.
// A command already running keeps the configuration it started with, so a
// concurrent refresh never sees a half-applied setting.
func (r *Runner) SetConfig(c Config) {
	r.cfgMu.Lock()
	r.cfg = c
	r.cfgMu.Unlock()
}

func (r *Runner) config() Config {
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	return r.cfg
}

// SetPath changes the Git executable and forgets the path resolved before, so
// the next command runs the new binary instead of the cached one.
func (r *Runner) SetPath(path string) {
	r.resolveMu.Lock()
	r.Path = path
	r.pathReady = false
	r.resolved = ""
	r.findErr = nil
	r.resolveMu.Unlock()
}

// Executable reports the Git binary commands would run: the configured Path,
// or the one found on PATH and the well-known macOS locations. The error is
// classified ErrNoGit when there is none. The settings dialog shows it so the
// user can see what "automatic" resolved to.
func (r *Runner) Executable() (string, error) {
	return r.git()
}

// ExecutableIn reports the Git binary the commands of the repository at root
// run: a WSL repository runs the git inside its distribution, anything else
// uses Executable. The settings dialog asks this so the echoed path names the
// binary that will really serve the open repository, not the Windows default.
//
// A distro this process has not probed yet is probed here; the answer is then
// cached like every other probe, so opening the dialog is cheap.
func (r *Runner) ExecutableIn(ctx context.Context, root string) (string, error) {
	if sshpath.Is(root) {
		if target, err := sshpath.Parse(root); err == nil {
			return "git (on " + target.Host + ")", nil
		}
	}
	distro := wslDistroOf(root)
	if distro == "" {
		return r.Executable()
	}
	if exe := r.cachedWSLGit(distro); exe != "" {
		return exe, nil
	}
	if err := r.probeWSL(ctx, distro); err != nil {
		return "", err
	}
	if exe := r.cachedWSLGit(distro); exe != "" {
		return exe, nil
	}
	// A probed distro always records its path; "git" is the lookup Git itself
	// performs through PATH inside the distro.
	return "git", nil
}

func (r *Runner) cachedWSLGit(distro string) string {
	r.wslMu.Lock()
	defer r.wslMu.Unlock()
	return r.wslGitPath[distro]
}

// CachedExecutableIn reports the Git binary the commands of the repository at
// root run without starting a process: a WSL repository answers with the git
// inside its distribution when an earlier probe recorded it, "" when this
// process has not probed the distro yet. It exists for callers that paint,
// where running wsl.exe — a round trip that can wait on the distribution
// booting — would stall the frame.
func (r *Runner) CachedExecutableIn(root string) (string, error) {
	if sshpath.Is(root) {
		if target, err := sshpath.Parse(root); err == nil {
			return "git (on " + target.Host + ")", nil
		}
	}
	distro := wslDistroOf(root)
	if distro == "" {
		return r.Executable()
	}
	return r.cachedWSLGit(distro), nil
}

func (r *Runner) setWSLGit(distro, exe string) {
	r.wslMu.Lock()
	if r.wslGitPath == nil {
		r.wslGitPath = map[string]string{}
	}
	if r.wslReady == nil {
		r.wslReady = map[string]bool{}
	}
	r.wslGitPath[distro] = exe
	r.wslReady[distro] = true
	r.wslMu.Unlock()
}

// SetCommandLog installs the hook that records finished commands. The
// application layer uses it to fill the console and the event log; nil turns
// recording off.
func (r *Runner) SetCommandLog(fn vcs.CommandLog) { r.Log = fn }

func (r *Runner) git() (string, error) {
	r.resolveMu.Lock()
	defer r.resolveMu.Unlock()
	if r.pathReady {
		return r.resolved, r.findErr
	}
	r.pathReady = true
	if r.Path != "" {
		r.resolved = r.Path
		return r.resolved, nil
	}
	p, err := exec.LookPath("git")
	if err != nil {
		// GUI processes on macOS start without the user's shell PATH.
		for _, c := range []string{"/opt/homebrew/bin/git", "/usr/local/bin/git", "/usr/bin/git"} {
			if st, e := os.Stat(c); e == nil && !st.IsDir() {
				r.resolved = c
				return r.resolved, nil
			}
		}
		r.findErr = &Error{Kind: ErrNoGit, Err: err}
		return "", r.findErr
	}
	r.resolved = p
	return r.resolved, nil
}

// baseArgs are prepended to every command: stable, uncolored, unquoted
// output regardless of the user's configuration.
var baseArgs = []string{"-c", "core.quotepath=false", "-c", "color.ui=false", "-c", "log.showSignature=false"}

func (r *Runner) command(ctx context.Context, dir string, args []string) (*exec.Cmd, error) {
	if sshpath.Is(dir) {
		// A remote repository is read-only in this version. The allowlist is
		// the last line of defence: the application also hides write actions,
		// but a command that slips through must never reach the server.
		if !remoteReadCommand(args) {
			return nil, &Error{Kind: ErrReadOnlyRemote, Args: args, Stderr: "remote repositories are read-only in Viff"}
		}
		target, err := sshpath.Parse(dir)
		if err != nil {
			return nil, &Error{Kind: ErrOther, Args: args, Err: err}
		}
		// Local Git settings (signing, credential helper, SSH command) point at
		// programs and paths on this machine; the server's own configuration
		// applies to a remote repository, as it does for a WSL checkout.
		full := make([]string, 0, len(baseArgs)+len(args))
		full = append(full, baseArgs...)
		full = append(full, args...)
		return r.ssh().Command(ctx, target, target.Path, full)
	}
	if wslpath.Is(dir) {
		// A WSL repository runs the distro's own git through wsl.exe; see
		// wsl_windows.go and docs/decisions/0038-wsl-repositories.md.
		return r.wslCommand(ctx, dir, args)
	}
	git, err := r.git()
	if err != nil {
		return nil, err
	}
	cfg := r.config()
	full := make([]string, 0, len(baseArgs)+len(args)+8)
	full = append(full, baseArgs...)
	full = append(full, configArgs(cfg)...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, git, full...)
	// A GUI build has no console; without this every Git command would open
	// and flash its own console window on Windows.
	process.HideConsoleWindow(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // a GUI has no terminal to prompt on
		"GIT_OPTIONAL_LOCKS=0",  // reads never take the index lock from an external tool
		"LC_ALL=C",
		"LANGUAGE=C",
	)
	cmd.Env = append(cmd.Env, r.Env...)
	if cfg.SSHCommand != "" {
		// After Env so the setting wins; Git hands this to its ssh invocation.
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+cfg.SSHCommand)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd, nil
}

// limitedBuffer keeps at most limit bytes.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

// Run executes git in dir and returns its stdout.
func (r *Runner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return r.RunInput(ctx, dir, nil, args...)
}

// RunInput executes git with stdin.
func (r *Runner) RunInput(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	return r.runInput(ctx, dir, stdin, false, 0, args...)
}

// answerExit optionally treats a nonzero Git exit status as a normal answer
// rather than logging a command failure. Cancellation is always an error.
func (r *Runner) runInput(ctx context.Context, dir string, stdin []byte, processTree bool, answerExit int, args ...string) ([]byte, error) {
	start := time.Now()
	cmd, err := r.command(ctx, dir, args)
	if err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if processTree {
		err = process.RunTree(cmd)
	} else {
		err = cmd.Run()
	}
	if err != nil {
		err = r.wrap(ctx, dir, args, err, stderr.buf.String(), stdout.String())
		if answerExit != 0 && ctx.Err() == nil && exitCodeOf(err) == answerExit {
			err = nil
		}
	}
	if r.Log != nil {
		r.Log(dir, args, time.Since(start), err)
	}
	return stdout.Bytes(), err
}

// RunProgress executes a long-running command and reports each line Git
// writes to stderr (progress output uses carriage returns; both count).
func (r *Runner) RunProgress(ctx context.Context, dir string, progress func(line string), args ...string) ([]byte, error) {
	start := time.Now()
	cmd, err := r.command(ctx, dir, args)
	if err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, r.wrap(ctx, dir, args, err, "", "")
	}
	stderr := &limitedBuffer{limit: stderrLimit}
	buf := make([]byte, 4096)
	var line []byte
	for {
		n, rerr := pipe.Read(buf)
		for _, c := range buf[:n] {
			if c == '\r' || c == '\n' {
				if len(line) > 0 && progress != nil {
					progress(string(line))
				}
				line = line[:0]
			} else {
				line = append(line, c)
			}
		}
		stderr.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	if len(line) > 0 && progress != nil {
		progress(string(line))
	}
	err = cmd.Wait()
	if err != nil {
		err = r.wrap(ctx, dir, args, err, stderr.buf.String(), stdout.String())
	}
	if r.Log != nil {
		r.Log(dir, args, time.Since(start), err)
	}
	return stdout.Bytes(), err
}

func (r *Runner) wrap(ctx context.Context, dir string, args []string, err error, stderr, stdout string) error {
	exit := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	}
	// wsl.exe writes its own diagnostics as UTF-16LE with a BOM while the Git
	// it execs passes UTF-8 through; the BOM decides (see wsl.go). Git's own
	// output never starts with one, so decoding unconditionally is safe.
	stderr, stdout = decodeWSLText(stderr), decodeWSLText(stdout)
	kind := classify(stderr+"\n"+stdout, exit, ctx.Err())
	// ssh's own failures can look like Git's: "Permission denied (publickey)"
	// matches the credential-helper pattern too. For an SSH repository the
	// transport classification wins unless the command was canceled.
	if ctx.Err() == nil && sshpath.Is(dir) {
		if sshKind, ok := classifySSH(stderr, stdout, exit); ok {
			kind = sshKind
		}
	}
	return &Error{Kind: kind, Args: args, ExitCode: exit, Stderr: stderr, Stdout: stdout, Err: err}
}

// classifySSH separates the failures that happen before the remote Git runs.
// ssh itself exits 255 when it cannot connect or the host key or key is
// rejected; a remote Git that fails for its own reasons exits with Git's code
// (128 and friends), so its messages are not stolen by these patterns.
func classifySSH(stderr, stdout string, exit int) (ErrorKind, bool) {
	s := strings.ToLower(stderr + "\n" + stdout)
	switch {
	case exit == 255 && strings.Contains(s, "host key verification failed"):
		return ErrSSHHostKey, true
	case exit == 255 && strings.Contains(s, "remote host identification has changed"):
		return ErrSSHHostKey, true
	case exit == 255 && strings.Contains(s, "no matching host key"):
		return ErrSSHHostKey, true
	case exit == 255 && strings.Contains(s, "permission denied (publickey"):
		return ErrSSHAuth, true
	case exit == 255 && strings.Contains(s, "permission denied (password"):
		return ErrSSHAuth, true
	case exit == 255 && strings.Contains(s, "permission denied (keyboard-interactive"):
		return ErrSSHAuth, true
	case exit == 255 && strings.Contains(s, "too many authentication failures"):
		return ErrSSHAuth, true
	case (exit == 127 || exit == 126) && (strings.Contains(s, "git: command not found") || strings.Contains(s, "git: not found") || strings.Contains(s, "git: no such file")):
		return ErrSSHGitMissing, true
	case exit == 255 && (strings.Contains(s, "could not resolve hostname") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection timed out") ||
		strings.Contains(s, "operation timed out") ||
		strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "no route to host") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "connection closed") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "kex_exchange_identification") ||
		strings.Contains(s, "timeout, server")):
		return ErrSSHConnect, true
	case exit == 255 && strings.Contains(s, "ssh:"):
		return ErrSSHConnect, true
	}
	return ErrOther, false
}

// Stream starts git and returns its stdout for incremental reading. Closing
// the stream stops the process.
func (r *Runner) Stream(ctx context.Context, dir string, args ...string) (*Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd, err := r.command(ctx, dir, args)
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, r.wrap(ctx, dir, args, err, "", "")
	}
	return &Stream{cmd: cmd, out: out, cancel: cancel, stderr: stderr, args: args, dir: dir, runner: r, ctx: ctx}, nil
}

// Stream is a running git process whose stdout is read incrementally.
type Stream struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	cancel context.CancelFunc
	stderr *limitedBuffer
	args   []string
	dir    string
	runner *Runner
	ctx    context.Context
	closed bool
}

func (s *Stream) Read(p []byte) (int, error) { return s.out.Read(p) }

// Close stops the process and returns its error, if it failed by itself.
func (s *Stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	err := s.cmd.Wait()
	if err != nil && s.ctx.Err() == nil {
		return s.runner.wrap(s.ctx, s.dir, s.args, err, s.stderr.buf.String(), "")
	}
	return nil
}

// Wait waits for a stream read to its end and returns the process error.
func (s *Stream) Wait() error {
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.cmd.Wait()
	if err != nil {
		err = s.runner.wrap(s.ctx, s.dir, s.args, err, s.stderr.buf.String(), "")
	}
	s.cancel()
	return err
}

// Version returns the output of git --version, e.g. "2.53.0".
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, "", "--version")
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	v = strings.TrimPrefix(v, "git version ")
	return v, nil
}
