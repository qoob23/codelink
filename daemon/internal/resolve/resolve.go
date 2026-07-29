// Package resolve maps a repository-relative path onto a concrete file inside a
// local checkout.
//
// The daemon never knows how deep a given candidate directory sits inside the
// repository: an nvim instance may be cwd'd at the repo root, at an
// intermediate directory, or deep inside one package. The algorithm below finds
// the longest suffix of the candidate directory that is also a prefix of the
// repo path AND that yields a file which actually exists on disk.
package resolve

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Segments splits a slash-separated repo path into its non-empty segments.
func Segments(repoPath string) []string {
	raw := strings.Split(filepath.ToSlash(repoPath), "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}

// dirSegments splits an absolute local directory into its non-empty segments.
func dirSegments(dir string) []string {
	return Segments(filepath.Clean(dir))
}

// Exists reports whether a path exists (file or directory).
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Resolve finds the local path for repoPath underneath the candidate directory
// candidateDir.
//
// It returns the resolved absolute path, the overlap k (how many leading
// segments of repoPath were already contained in candidateDir), and whether a
// path was found.
//
// k is capped at len(P)-1 so the final segment of repoPath (the file name) is
// never consumed by the overlap; without that cap a candidate directory that
// happens to equal the full repo path would "resolve" to itself, i.e. to a
// directory rather than the requested file.
//
// Iteration goes from the deepest overlap downwards and requires the candidate
// to exist, so when several overlaps are plausible the deepest existing one
// wins — that is where the user is actually working.
func Resolve(repoPath, candidateDir string) (string, int, bool) {
	p := Segments(repoPath)
	if len(p) == 0 || candidateDir == "" {
		return "", 0, false
	}
	// A repo path is always relative and downward. Refusing ".." here stops a
	// traversal before filepath.Join silently normalises it into an escape;
	// the root allowlist would also catch it, but defence in depth is cheap.
	if slices.Contains(p, "..") {
		return "", 0, false
	}
	c := dirSegments(candidateDir)

	kmax := min(len(c), len(p)-1)
	for k := kmax; k >= 0; k-- {
		if !slices.Equal(c[len(c)-k:], p[:k]) {
			continue
		}
		cand := filepath.Join(append([]string{candidateDir}, p[k:]...)...)
		if Exists(cand) {
			return cand, k, true
		}
	}
	return "", 0, false
}

// Project returns the longest prefix of repoPath that ends immediately BEFORE
// the first segment named in markers, plus that prefix's last segment.
//
// For "src/pkg/widget/lib/main.go" with markers ["lib"] the first marker
// segment is "lib" at index 3, so this yields ("src/pkg/widget", "widget").
// Note that markers are matched in path order, so a markers list that also
// contained "src" would instead match at index 0 and take the fallback below.
//
// When no marker appears (or one appears at the very front) it falls back to
// the directory containing the file.
func Project(repoPath string, markers []string) (string, string) {
	p := Segments(repoPath)
	if len(p) == 0 {
		return "", ""
	}
	idx := -1
	for i, seg := range p {
		if slices.Contains(markers, seg) {
			idx = i
			break
		}
	}
	var project string
	if idx > 0 {
		project = path.Join(p[:idx]...)
	} else {
		project = path.Dir(path.Join(p...))
		if project == "." {
			project = ""
		}
	}
	if project == "" {
		return "", ""
	}
	return project, path.Base(project)
}

// Canonical returns the physical (symlink-resolved, cleaned) form of p.
//
// When p does not fully exist yet, the deepest existing ancestor is resolved
// and the remainder re-attached. A bare Clean fallback would be inconsistent:
// an existing root canonicalises through its symlinks while a not-yet-existing
// path underneath it would not, so a containment test between the two could
// disagree with itself — and containment here is a security control.
//
// This matters on this machine in two places:
//   - the registry's launch_cwd/cwd come from vim.fn.getcwd(), which is already
//     PHYSICAL, whereas Ghostty's "working directory" comes from OSC 7 and is
//     the shell's LOGICAL $PWD. Comparing them verbatim silently misses on any
//     symlinked directory (~/.config is a stow symlink here, so this is live).
//   - the root allowlist, where a symlinked alias of an allowed root must not
//     be treated as a different, disallowed directory.
func Canonical(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(filepath.Clean(r), rest)
		}
		cur = parent
	}
}

// SameDir reports whether two directory paths denote the same directory once
// symlinks are resolved on both sides.
func SameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	return Canonical(a) == Canonical(b)
}

// Within reports whether child is at or below parent, after cleaning both.
// It is a pure lexical check; callers that need symlink safety must call
// filepath.EvalSymlinks themselves first.
func Within(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
