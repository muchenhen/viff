package gitcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"viff/internal/domain"
	"viff/internal/sshpath"
	"viff/internal/wslpath"
)

// LFSStatus is what Viff knows about Git LFS in a repository. Viff detects LFS
// and shows its facts; it never installs, configures or manages it (ADR 0031).
//
// A rule can establish LFS usage without enumerating files. No rules found
// leaves file usage unknown until the user requests a listing.
type LFSStatus struct {
	// Installed reports that `git lfs version` succeeded. A missing git-lfs is
	// an expected state, not an error.
	Installed bool
	// Version is the first line of `git lfs version`, e.g. "git-lfs/3.5.1
	// (GitHub; darwin arm64; go 1.22.3)".
	Version string
	// Patterns are the .gitattributes patterns whose filter attribute selects
	// lfs, in file order. Patterns from a nested .gitattributes are prefixed
	// with that file's directory so they read as repository-relative.
	Patterns []string
	// Managed reports that an attribute rule selects LFS.
	Managed bool
	// Err records why this snapshot is incomplete: an unreadable
	// .gitattributes or failure to discover the attributes files.
	// Callers log the classified code, never the message.
	Err error
}

// NeedsInstall reports that the repository uses LFS rules but this machine has
// no git-lfs. It is the condition for the UI's warning.
func (st LFSStatus) NeedsInstall() bool {
	return !st.Installed && len(st.Patterns) > 0
}

// Limits keep a pathological repository from turning detection into an
// unbounded read or an unbounded pattern list.
const (
	lfsAttributesMaxSize = 1 << 20 // 1 MiB per .gitattributes file
	lfsPatternLimit      = 512
)

// LFSInputs identifies the HEAD and staged changes that a file listing reads.
// The index key hashes the delta from HEAD, avoiding a full tracked-file list.
type LFSInputs struct {
	Head  HeadState
	Index [sha256.Size]byte
}

// LFSInputs reads the listing's inputs without scanning the working tree.
func (r *Runner) LFSInputs(ctx context.Context, repo domain.Repo) (LFSInputs, error) {
	head, err := r.Head(ctx, repo)
	if err != nil {
		return LFSInputs{}, fmt.Errorf("read Git LFS HEAD: %w", err)
	}
	args := []string{"diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--ignore-submodules=none"}
	if head.Head != "" {
		args = append(args, head.Head)
	}
	out, err := r.Run(ctx, repo.Root, append(args, "--")...)
	if err != nil {
		return LFSInputs{}, fmt.Errorf("read Git LFS index changes: %w", err)
	}
	return LFSInputs{Head: head, Index: sha256.Sum256(out)}, nil
}

// lfsCapability probes `git lfs version` once per Git executable and process.
// The probe runs in the repository so a WSL repository probes the distro's
// git-lfs, not the Windows one; the cache key follows the executable that
// would serve the repo. A canceled probe is not cached: it says nothing about
// the installation.
func (r *Runner) lfsCapability(ctx context.Context, repo domain.Repo) (string, bool) {
	key := "custom"
	if r.LFSCommand == nil {
		if distro := wslDistroOf(repo.Root); distro != "" {
			key = "wsl:" + distro
		} else if sshpath.Is(repo.Root) {
			if target, err := sshpath.Parse(repo.Root); err == nil {
				key = "ssh:" + target.String()
			} else {
				key = "ssh:" + repo.Root
			}
		} else {
			git, err := r.git()
			if err != nil {
				// Without git no repository can be opened in the first place.
				return "", false
			}
			key = git
		}
	}
	r.lfsMu.Lock()
	if r.lfsProbed && r.lfsProbeKey == key {
		version, installed := r.lfsVersion, r.lfsInstalled
		r.lfsMu.Unlock()
		return version, installed
	}
	r.lfsMu.Unlock()

	out, err := r.lfsCommand(ctx, repo.Root, "version")
	if ctx.Err() != nil {
		return "", false
	}
	installed := err == nil
	version := ""
	if installed {
		version = lfsVersionLine(out)
	}
	r.lfsMu.Lock()
	r.lfsProbeKey, r.lfsProbed = key, true
	r.lfsInstalled, r.lfsVersion = installed, version
	r.lfsMu.Unlock()
	return version, installed
}

// lfsCommand runs a git-lfs subcommand. LFSCommand is the test seam: it lets a
// test simulate git-lfs being installed, missing or broken without the binary.
func (r *Runner) lfsCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if r.LFSCommand != nil {
		return r.LFSCommand(ctx, dir, args...)
	}
	full := append([]string{"lfs"}, args...)
	if len(args) > 0 && args[0] == "ls-files" {
		if (sshpath.Is(dir) && r.SSH == nil) || wslpath.Is(dir) {
			return r.lfsTransportFiles(ctx, dir, full)
		}
		return r.runInput(ctx, dir, nil, true, 0, full...)
	}
	return r.Run(ctx, dir, full...)
}

func lfsVersionLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const limit = 160
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

// LFSFiles enumerates managed files on explicit request.
func (r *Runner) LFSFiles(ctx context.Context, repo domain.Repo) ([]string, error) {
	// Prefer NUL records, falling back to JSON for releases such as 3.5.1.
	// Newline records cannot count a file containing a newline correctly.
	out, err := r.lfsCommand(ctx, repo.Root, "ls-files", "--name-only", "-z")
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrLFSReapUnconfirmed) {
		out, err = r.lfsCommand(ctx, repo.Root, "ls-files", "--json")
		if err == nil {
			return parseLFSJSON(out)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("list Git LFS files: %w", err)
	}
	return parseLFSFiles(out), nil
}

func parseLFSJSON(out []byte) ([]string, error) {
	var result struct {
		Files json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		// SyntaxError may quote repository bytes, so expose a fixed message.
		return nil, errors.New("invalid Git LFS file-list JSON")
	}
	if result.Files == nil {
		return nil, errors.New("Git LFS file-list JSON has no files array")
	}
	// Git LFS 3.5.1 encodes its nil slice as null for a successful zero.
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result.Files, &entries); err != nil {
		return nil, errors.New("invalid Git LFS files array")
	}
	files := make([]string, 0, len(entries))
	for _, file := range entries {
		if file.Name == "" {
			return nil, errors.New("Git LFS file-list JSON has an empty name")
		}
		files = append(files, file.Name)
	}
	return files, nil
}

// parseLFSFiles splits `git lfs ls-files --name-only` output. Newer git-lfs
// writes NUL-separated paths for -z; an older or shimmed one may write lines,
// so both are accepted without trimming anything from a path.
func parseLFSFiles(out []byte) []string {
	var files []string
	if bytes.IndexByte(out, 0) >= 0 {
		for _, rec := range bytes.Split(out, []byte{0}) {
			if len(rec) > 0 {
				files = append(files, string(rec))
			}
		}
		return files
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

// LFSRuleCache contains immutable parse results for one working tree. Only
// current candidates are retained, and the total number of cached patterns
// (including duplicates) is bounded by the display limit.
type LFSRuleCache map[string]lfsAttributeRules

type lfsAttributeRules struct {
	digest   [sha256.Size]byte
	patterns []string
}

const (
	lfsRuleCacheEntries = 512
	lfsRuleCacheBytes   = 4 << 20
)

// LFS detects rules and tool availability using the supplied attribute paths.
// A failed read returns no replacement snapshot. The caller owns its cache.
func (r *Runner) LFS(ctx context.Context, repo domain.Repo, paths []string, previous LFSRuleCache) (LFSStatus, LFSRuleCache) {
	version, installed := r.lfsCapability(ctx, repo)
	st := LFSStatus{Installed: installed, Version: version}
	paths = append([]string{".gitattributes"}, paths...)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	next := make(LFSRuleCache)
	seen := make(map[string]bool)
	budget := lfsPatternLimit
	byteBudget := lfsRuleCacheBytes
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			st.Err = err
			return st, nil
		}
		data, err := r.readLFSAttributes(ctx, repo, rel)
		if err != nil {
			st.Err = err
			return st, nil
		}
		digest := sha256.Sum256(data)
		rules, ok := previous[rel]
		if !ok || rules.digest != digest {
			rules = lfsAttributeRules{digest, parseLFSAttributes(data, lfsAttributeDir(rel))}
		}
		for _, p := range rules.patterns {
			if seen[p] {
				continue
			}
			seen[p] = true
			st.Patterns = append(st.Patterns, p)
			if len(st.Patterns) == lfsPatternLimit {
				break
			}
		}
		size := len(rel)
		for _, pattern := range rules.patterns {
			size += len(pattern)
		}
		if len(next) < lfsRuleCacheEntries && len(rules.patterns) <= budget && size <= byteBudget {
			next[rel] = rules
			budget -= len(rules.patterns)
			byteBudget -= size
		}
		if len(st.Patterns) == lfsPatternLimit {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		st.Err = err
		return st, nil
	}
	st.Managed = len(st.Patterns) > 0
	return st, next
}

// readLFSAttributes reads one .gitattributes relative to the repository root.
// A missing file is not an error: most repositories have none. A remote
// repository reads the file through its SSH transport, so LFS detection uses
// the server's attributes rather than this machine's checkout.
func (r *Runner) readLFSAttributes(ctx context.Context, repo domain.Repo, rel string) ([]byte, error) {
	if repo.IsSSH() {
		c, err := r.Read(ctx, repo, RevWorktree, rel)
		if err != nil {
			return nil, fmt.Errorf("read .gitattributes: %w", err)
		}
		if !c.Exists || c.Symlink || c.Submodule {
			return nil, nil
		}
		if c.Size > lfsAttributesMaxSize || len(c.Data) > lfsAttributesMaxSize {
			return nil, fmt.Errorf("read .gitattributes: file is larger than %d bytes", lfsAttributesMaxSize)
		}
		return c.Data, nil
	}
	p := filepath.Join(repo.Root, filepath.FromSlash(rel))
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read .gitattributes: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	if info.Size() > lfsAttributesMaxSize {
		return nil, fmt.Errorf("read .gitattributes: file is larger than %d bytes", lfsAttributesMaxSize)
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read .gitattributes: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, lfsAttributesMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read .gitattributes: %w", err)
	}
	if len(data) > lfsAttributesMaxSize {
		return nil, fmt.Errorf("read .gitattributes: file is larger than %d bytes", lfsAttributesMaxSize)
	}
	return data, nil
}

// lfsAttributeDir returns the repository-relative directory a .gitattributes
// file lives in, "" for the root file. Patterns in a nested file apply there.
func lfsAttributeDir(rel string) string {
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	return dir
}

// parseLFSAttributes returns the patterns whose final filter attribute is lfs.
// Within one file the last assignment wins, matching how Git reads a repeated
// pattern; patterns from different files are scoped by dir and merged by the
// caller.
func parseLFSAttributes(data []byte, dir string) []string {
	var order []string
	state := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := splitAttributeFields(trimmed)
		if len(fields) < 2 {
			continue
		}
		pattern := lfsAttributePattern(dir, fields[0])
		if pattern == "" {
			continue
		}
		mark := func() {
			if _, ok := state[pattern]; !ok {
				order = append(order, pattern)
			}
		}
		for _, attr := range fields[1:] {
			switch {
			case strings.HasPrefix(attr, "filter="):
				mark()
				state[pattern] = strings.TrimPrefix(attr, "filter=") == "lfs"
			case attr == "filter" || attr == "-filter" || attr == "!filter":
				// `filter` sets the attribute to true (not the lfs driver) and
				// the other two unset it; none of them selects Git LFS.
				mark()
				state[pattern] = false
			}
		}
	}
	var out []string
	for _, p := range order {
		if state[p] {
			out = append(out, p)
		}
	}
	return out
}

func lfsAttributePattern(dir, pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	if dir == "" {
		return pattern
	}
	return dir + "/" + strings.TrimPrefix(pattern, "/")
}

// splitAttributeFields splits one attributes line. A pattern with spaces is
// C-quoted (`"model final.bin"`); backslash escapes the next byte, and tabs
// separate fields as well as spaces. Attribute values are ordinary fields.
func splitAttributeFields(line string) []string {
	var fields []string
	var cur strings.Builder
	started, quoted, escaped := false, false, false
	flush := func() {
		if started {
			fields = append(fields, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			switch c {
			case 'n':
				cur.WriteByte('\n')
			case 't':
				cur.WriteByte('\t')
			case 'r':
				cur.WriteByte('\r')
			default:
				cur.WriteByte(c)
			}
			escaped = false
			started = true
			continue
		}
		switch {
		case c == '\\':
			escaped = true
			started = true
		case c == '"':
			quoted = !quoted
			started = true
		case (c == ' ' || c == '\t') && !quoted:
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return fields
}
