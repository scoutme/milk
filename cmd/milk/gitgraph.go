package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Git data fetch and code-laid commit-graph layout for the git panel
// (issue #206, ADR-0066). The lane layout is computed here from `git log`
// plumbing (parent hashes) rather than by parsing git's own ASCII --graph
// output — every row's commit hash is known directly, which is exactly what
// click-to-preview needs. Pure functions (parseGitLog, layoutGraph,
// parseStatusBranch) are separated from the process calls so they are unit
// testable without a repo.

// gitCommit is one commit from `git log` plumbing.
type gitCommit struct {
	Short   string   // %h
	Hash    string   // %H
	Parents []string // %P, full hashes
	Subject string   // %s
	Decor   string   // %D, e.g. "HEAD -> main, origin/main"
	Age     string   // %ar
}

// gitGraphLine is one rendered graph row: the lane prefix and the commit it
// belongs to (always set — one row per commit).
type gitGraphLine struct {
	Prefix string // lane cells, e.g. "●─╮"
	Commit gitCommit
}

const (
	gitGraphMaxCommits = 200
	gitGraphTimeout    = 3 * time.Second
	gitShowMaxBytes    = 128 * 1024
)

// parseGitLog parses `git log --pretty=format:%h%x1f%H%x1f%P%x1f%s%x1f%D%x1f%ar`
// output into commits, in git's own order (newest first).
func parseGitLog(out string) []gitCommit {
	var commits []gitCommit
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 6 {
			continue
		}
		var parents []string
		for _, p := range strings.Fields(f[2]) {
			parents = append(parents, p)
		}
		commits = append(commits, gitCommit{
			Short:   f[0],
			Hash:    f[1],
			Parents: parents,
			Subject: f[3],
			Decor:   f[4],
			Age:     f[5],
		})
	}
	return commits
}

// layoutGraph lays out one row per commit, newest first, drawing git-style
// lanes: a lane continues straight down (│), a commit is a dot (●), a merge
// edge from the commit to a second parent's lane turns the corner (╮/╭) and
// a lane that merges back into this commit ends here (╯/╰). Lane assignment:
// a commit whose hash is already expected by a lane lands there; otherwise it
// opens the leftmost free lane (or appends). The first parent takes over the
// commit's lane; every other parent opens or reuses its own lane. A lane
// holding a hash that is being printed merges in and is freed.
func layoutGraph(commits []gitCommit) []gitGraphLine {
	var lanes []string // expected commit hash per lane, "" = free
	var out []gitGraphLine

	place := func(hash string) int {
		for i, h := range lanes {
			if h == hash {
				return i
			}
		}
		if i := slices.Index(lanes, ""); i >= 0 {
			lanes[i] = hash
			return i
		}
		lanes = append(lanes, hash)
		return len(lanes) - 1
	}

	for _, c := range commits {
		col := place(c.Hash)

		// Parents: first parent keeps the commit's lane, the rest fan out.
		// Placed before the row is sized below — a fan-out may open a new lane.
		var fanout []int
		seen := map[string]bool{}
		for i, p := range c.Parents {
			if seen[p] {
				continue
			}
			seen[p] = true
			if i == 0 {
				lanes[col] = p
				continue
			}
			fanout = append(fanout, place(p))
		}

		glyphs := make([]rune, len(lanes))
		gaps := make([]rune, max(len(lanes)-1, 0))
		for i := range glyphs {
			glyphs[i] = ' '
		}
		for i := range gaps {
			gaps[i] = ' '
		}
		for i, h := range lanes {
			if h != "" && i != col {
				glyphs[i] = '│'
			}
		}
		glyphs[col] = '●'

		// Horizontal edge from the commit to a lane at index to.
		edge := func(to int) {
			if to == col || to < 0 || to >= len(lanes) {
				return
			}
			lo, hi := col, to
			if lo > hi {
				lo, hi = hi, lo
			}
			for g := lo; g < hi && g < len(gaps); g++ {
				gaps[g] = '─'
			}
			if to > col {
				glyphs[to] = '╮'
			} else {
				glyphs[to] = '╭'
			}
		}

		for _, to := range fanout {
			edge(to)
		}

		// Any other lane expecting this commit merges in here and is freed.
		for i, h := range lanes {
			if i != col && h == c.Hash {
				lo, hi := col, i
				if lo > hi {
					lo, hi = hi, lo
				}
				for g := lo; g < hi && g < len(gaps); g++ {
					gaps[g] = '─'
				}
				if i > col {
					glyphs[i] = '╯'
				} else {
					glyphs[i] = '╰'
				}
				lanes[i] = ""
			}
		}

		var sb strings.Builder
		for i := range glyphs {
			sb.WriteRune(glyphs[i])
			if i < len(gaps) {
				sb.WriteRune(gaps[i])
			}
		}
		out = append(out, gitGraphLine{Prefix: strings.TrimRight(sb.String(), " "), Commit: c})
	}
	return out
}

// parseStatusBranch reads `git status --porcelain -b` output: the branch from
// the "## …" header line (with upstream/ahead markers stripped) and the count
// of dirty entries after it. Detached HEAD reports "HEAD".
func parseStatusBranch(out string) (branch string, dirty int) {
	branch = "HEAD"
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			b := strings.TrimPrefix(line, "## ")
			if i := strings.IndexAny(b, ". "); i >= 0 { // "main...origin/main [ahead 1]" → "main"
				b = b[:i]
			}
			if b != "" {
				branch = b
			}
			continue
		}
		dirty++
	}
	return branch, dirty
}

// gitErrDefinitive reports whether a git process failure is a permanent
// statement about the environment — git is not installed, or cwd is not
// inside a work tree — as opposed to a retryable hiccup: the 3s timeout
// under load or mid-checkout (killed process, empty stderr), lock
// contention, a vanished cwd. Only the definitive class may be rendered as
// "not a git repository" or cached as "no repo here"; a transient one must
// keep the last good snapshot and retry on the next poll (the git panel
// used to flash "not a git repository" exactly this way).
func gitErrDefinitive(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true // LookPath failed — the git binary itself is missing
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return bytes.Contains(ee.Stderr, []byte("not a git repository"))
	}
	return false // context deadline, signal kill, I/O error — all retryable
}

// transientGitErr is the complement: a failure worth retrying next tick.
func transientGitErr(err error) bool { return err != nil && !gitErrDefinitive(err) }

// gitLog runs the graph's data fetch in cwd. Returns the parsed commits and
// the status info; err is non-nil when git is missing, cwd is not a repo,
// or the repo has no commits yet (the last is not an error — commits comes
// back as an empty, non-nil slice so the panel can say "(no commits)").
// A transient failure (timeout/lock) is returned as err too — classify it
// with transientGitErr before rendering.
func gitLog(cwd string) (commits []gitCommit, branch string, dirty int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitGraphTimeout)
	defer cancel()

	log := exec.CommandContext(ctx, "git", "-C", cwd, "log",
		"-n", "200",
		"--pretty=format:%h\x1f%H\x1f%P\x1f%s\x1f%D\x1f%ar")
	out, err := log.Output()
	if err != nil {
		// A freshly-inited repo with no commits: git log refuses with a
		// fatal on stderr, but the repo is real — fall through to the
		// status probe with an empty (non-nil) commit list.
		var ee *exec.ExitError
		if !errors.As(err, &ee) || !bytes.Contains(ee.Stderr, []byte("does not have any commits yet")) {
			return nil, "", 0, err
		}
		commits = []gitCommit{}
	} else {
		commits = parseGitLog(string(out))
	}

	branch = "HEAD"
	// --no-optional-locks, same as the changeset probe: a panel refresh must
	// never wait on (or fail against) the index.lock of a checkout or commit
	// running concurrently — status errors are swallowed below, but a lock
	// hit would otherwise reset the header to "HEAD/0" every poll.
	status := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain", "-b", "--no-optional-locks")
	if sOut, sErr := status.Output(); sErr == nil {
		branch, dirty = parseStatusBranch(string(sOut))
	}
	return commits, branch, dirty, nil
}

// gitShow renders a commit's summary and diff (capped) for the preview view.
// hash is validated to be hexadecimal so this never passes user text to git.
func gitShow(cwd, hash string) (string, error) {
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", errBadHash
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitGraphTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "show", "--stat", "--patch", "--find-renames", hash)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := string(out)
	if len(s) > gitShowMaxBytes {
		s = s[:gitShowMaxBytes] + "\n… (diff truncated at 128 KB)"
	}
	return s, nil
}

// errBadHash rejects non-hex input to gitShow.
var errBadHash = &hashError{}

type hashError struct{}

func (*hashError) Error() string { return "invalid commit hash" }
