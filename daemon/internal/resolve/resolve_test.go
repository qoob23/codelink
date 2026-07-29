package resolve

import (
	"os"
	"path/filepath"
	"testing"
)

// mkfile creates parent dirs and an empty file under root, returning its path.
func mkfile(t *testing.T, root string, rel string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdirall %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("writefile %s: %v", p, err)
	}
	return p
}

func mkdir(t *testing.T, root string, rel string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdirall %s: %v", p, err)
	}
	return p
}

// deepRepoPath is a deliberately deep, generic repo-relative path: eight
// segments, so a candidate directory can overlap it at several different
// depths and the k values below are meaningful.
const deepRepoPath = "owner/repo/pkg/widget/lib/src/inner/x.go"

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		// setup builds the tree under tmp and returns (candidateDir, wantPath).
		// wantPath == "" means the resolve must miss.
		setup    func(t *testing.T, tmp string) (string, string)
		repoPath string
		wantK    int
		wantOK   bool
	}{
		{
			name:     "deep package cwd, k=4",
			repoPath: deepRepoPath,
			wantK:    4,
			wantOK:   true,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "checkout/main/owner/repo/pkg/widget")
				want := mkfile(t, c, "lib/src/inner/x.go")
				return c, want
			},
		},
		{
			name:     "repo root cwd, k=0",
			repoPath: deepRepoPath,
			wantK:    0,
			wantOK:   true,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "checkout/main")
				want := mkfile(t, c, deepRepoPath)
				return c, want
			},
		},
		{
			name:     "one level in, k=1",
			repoPath: deepRepoPath,
			wantK:    1,
			wantOK:   true,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "checkout/main/owner")
				want := mkfile(t, c, "repo/pkg/widget/lib/src/inner/x.go")
				return c, want
			},
		},
		{
			name:     "unrelated directory misses",
			repoPath: deepRepoPath,
			wantOK:   false,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "somewhere/unrelated")
				mkfile(t, c, "README.md")
				return c, ""
			},
		},
		{
			name:     "segment overlap but file absent on disk",
			repoPath: deepRepoPath,
			wantOK:   false,
			setup: func(t *testing.T, tmp string) (string, string) {
				// The cwd looks like a perfect k=4 match but nothing is there.
				c := mkdir(t, tmp, "checkout/other/owner/repo/pkg/widget")
				return c, ""
			},
		},
		{
			name:     "ambiguity: largest existing k wins",
			repoPath: "owner/repo/x.go",
			wantK:    1,
			wantOK:   true,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "checkout/main/owner")
				// k=1 candidate
				want := mkfile(t, c, "repo/x.go")
				// k=0 candidate — also exists, must lose.
				mkfile(t, c, "owner/repo/x.go")
				return c, want
			},
		},
		{
			name:     "kmax cap prevents degenerate cand == C",
			repoPath: "a/b",
			wantOK:   false,
			setup: func(t *testing.T, tmp string) (string, string) {
				// C ends with exactly the full repo path. Without the
				// len(P)-1 cap, k=2 would "resolve" to C itself (a directory).
				c := mkdir(t, tmp, "a/b")
				return c, ""
			},
		},
		{
			name:     "single-segment repo path resolves at k=0",
			repoPath: "README.md",
			wantK:    0,
			wantOK:   true,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "checkout/main")
				want := mkfile(t, c, "README.md")
				return c, want
			},
		},
		{
			name:     "directory target resolves too",
			repoPath: "owner/repo/pkg/widget/lib",
			wantK:    4,
			wantOK:   true,
			setup: func(t *testing.T, tmp string) (string, string) {
				c := mkdir(t, tmp, "checkout/main/owner/repo/pkg/widget")
				want := mkdir(t, c, "lib")
				return c, want
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			c, want := tc.setup(t, tmp)
			got, k, ok := Resolve(tc.repoPath, c)
			if ok != tc.wantOK {
				t.Fatalf("Resolve(%q, %q) ok=%v want %v (got %q)", tc.repoPath, c, ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if k != tc.wantK {
				t.Errorf("k = %d, want %d", k, tc.wantK)
			}
			if got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
			if got == filepath.Clean(c) {
				t.Errorf("resolved to the candidate directory itself: %q", got)
			}
		})
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	c := mkdir(t, tmp, "checkout/main")
	mkfile(t, tmp, "secret.txt")
	for _, rp := range []string{
		"../secret.txt",
		"../../etc/passwd",
		"pkg/../../secret.txt",
		"../../../../etc/passwd",
	} {
		if got, _, ok := Resolve(rp, c); ok {
			t.Errorf("Resolve(%q, %q) escaped the candidate dir to %q", rp, c, got)
		}
	}
}

func TestResolveEmptyInputs(t *testing.T) {
	if _, _, ok := Resolve("", t.TempDir()); ok {
		t.Error("empty repoPath must not resolve")
	}
	if _, _, ok := Resolve("a/b.go", ""); ok {
		t.Error("empty candidate dir must not resolve")
	}
}

func TestProject(t *testing.T) {
	markers := []string{"lib", "test", "bin", "src", "example", "tool", "integration_test"}
	tests := []struct {
		repoPath string
		project  string
		name     string
	}{
		{deepRepoPath, "owner/repo/pkg/widget", "widget"},
		{"owner/repo/pkg/widget/test/a_test.go", "owner/repo/pkg/widget", "widget"},
		// No marker anywhere: falls back to the file's own directory, which for
		// a package manifest is still the project dir.
		{"owner/repo/pkg/widget/manifest.yaml", "owner/repo/pkg/widget", "widget"},
		{"a/b/c.txt", "a/b", "b"},
		{"top.txt", "", ""},
		// A marker in first position degenerates to the parent dir.
		{"lib/x.go", "lib", "lib"},
		{"", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.repoPath, func(t *testing.T) {
			p, n := Project(tc.repoPath, markers)
			if p != tc.project || n != tc.name {
				t.Errorf("Project(%q) = (%q, %q), want (%q, %q)", tc.repoPath, p, n, tc.project, tc.name)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		parent, child string
		want          bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/../../etc/passwd", false},
	}
	for _, tc := range tests {
		if got := Within(tc.parent, tc.child); got != tc.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}

func TestCanonicalIsConsistentForNonExistentLeaves(t *testing.T) {
	// t.TempDir() on macOS lives under /var/folders, itself a symlink to
	// /private/var/folders — so this exercises the real asymmetry.
	tmp := t.TempDir()
	root := mkdir(t, tmp, "checkout")
	existing := mkfile(t, root, "lib/there.go")
	missing := filepath.Join(root, "lib", "not-there.go")

	croot := Canonical(root)
	if !Within(croot, Canonical(existing)) {
		t.Errorf("existing path %q not within canonical root %q", Canonical(existing), croot)
	}
	// The regression: a path that does not exist yet must canonicalise the same
	// way as one that does, or a containment check silently disagrees.
	if !Within(croot, Canonical(missing)) {
		t.Errorf("non-existent path %q not within canonical root %q", Canonical(missing), croot)
	}
	if Canonical(filepath.Dir(missing)) != Canonical(filepath.Dir(existing)) {
		t.Errorf("same directory canonicalised two ways: %q vs %q",
			Canonical(filepath.Dir(missing)), Canonical(filepath.Dir(existing)))
	}
	// Traversal must still not escape.
	if Within(croot, Canonical(filepath.Join(root, "..", "..", "etc", "passwd"))) {
		t.Error("traversal escaped the root after canonicalisation")
	}
}
