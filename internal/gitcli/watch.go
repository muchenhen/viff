package gitcli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"viff/internal/domain"
)

// WorktreeChangesRelevant checks one bounded batch of watcher paths. Git's
// default index-aware mode also treats directories containing tracked entries
// as visible, which matters when a whole directory is moved or removed.
func (r *Runner) WorktreeChangesRelevant(ctx context.Context, repo domain.Repo, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}
	var input bytes.Buffer
	for _, name := range paths {
		// These rule sources can affect status and LFS even if they are
		// ignored. Win32 may also report an ambiguous DOS short name.
		base := path.Base(name)
		if strings.EqualFold(base, ".gitignore") || strings.EqualFold(base, ".gitattributes") ||
			strings.ContainsRune(name, '~') || strings.HasPrefix(name, ":") {
			return true, nil
		}
		input.WriteString(name)
		input.WriteByte(0)
	}
	// A status of 1 means none of the paths are ignored, a successful probe.
	out, err := r.runInput(ctx, repo.Root, input.Bytes(), false, 1, "check-ignore", "-z", "--stdin")
	if err != nil {
		return false, fmt.Errorf("check worktree notification paths: %w", err)
	}
	// Count distinct exact path matches. An unfamiliar or partial answer
	// cannot suppress a refresh.
	ignored := make(map[string]bool, len(paths))
	for line := range bytes.SplitSeq(out, []byte{0}) {
		if len(line) != 0 {
			ignored[string(line)] = true
		}
	}
	args := make([]string, 0, 4+len(paths))
	args = append(args, "ls-files", "--cached", "-z", "--")
	for _, path := range paths {
		if !ignored[path] {
			return true, nil
		}
		// check-ignore looks up index entries by exact spelling, even on
		// Windows. Its ignore match can hide a tracked case alias. Git's
		// icase pathspecs fold ASCII; conservatively keep non-ASCII aliases.
		if utf8.RuneCountInString(path) != len(path) {
			return true, nil
		}
		args = append(args, ":(icase,literal)"+path)
	}
	s, err := r.Stream(ctx, repo.Root, args...)
	if err != nil {
		return false, fmt.Errorf("check tracked worktree path aliases: %w", err)
	}
	defer s.Close()
	// One byte is enough. A directory alias can match a large index subtree;
	// do not keep that listing in memory.
	var first [1]byte
	n, err := s.Read(first[:])
	if n > 0 {
		return true, nil
	}
	if err == io.EOF {
		err = s.Wait()
	}
	if err != nil {
		return false, fmt.Errorf("check tracked worktree path aliases: %w", err)
	}
	return false, nil
}
