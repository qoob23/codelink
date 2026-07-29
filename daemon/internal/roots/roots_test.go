package roots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"codelink/internal/providers"
)

func names(rs []Root) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, filepath.Base(r.Path))
	}
	slices.Sort(out)
	return out
}

// TestExpandBranchSymmetry is the regression for the two asymmetric branches:
// the "path" branch used to accept a regular file as a root, and the "glob"
// branch used to drop a symlink-to-directory because it stat'ed with Lstat.
func TestExpandBranchSymmetry(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "wt")
	if err := os.MkdirAll(filepath.Join(real, "realdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink to a directory: must be ACCEPTED by both branches.
	target := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(real, "linkdir")); err != nil {
		t.Fatal(err)
	}
	// A regular file: must be REJECTED by both branches.
	if err := os.WriteFile(filepath.Join(real, "notadir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dotfile: skipped by the glob filter.
	if err := os.WriteFile(filepath.Join(real, ".envrc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(t.TempDir())

	globbed := m.Expand(&providers.Provider{
		Roots: []providers.RootSpec{{Glob: filepath.Join(real, "*")}},
	})
	got := names(globbed)
	want := []string{"linkdir", "realdir"}
	if !slices.Equal(got, want) {
		t.Errorf("glob branch = %v, want %v (symlinked dir must be kept, file and dotfile dropped)", got, want)
	}

	// Same three candidates, now via the "path" branch.
	pathed := m.Expand(&providers.Provider{
		Roots: []providers.RootSpec{
			{Path: filepath.Join(real, "realdir")},
			{Path: filepath.Join(real, "linkdir")},
			{Path: filepath.Join(real, "notadir")},
			{Path: filepath.Join(real, "does-not-exist")},
		},
	})
	got = names(pathed)
	if !slices.Equal(got, want) {
		t.Errorf("path branch = %v, want %v (a regular file must not be accepted as a root)", got, want)
	}
}

func TestExpandDeduplicates(t *testing.T) {
	base := t.TempDir()
	d := filepath.Join(base, "only")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.TempDir())
	out := m.Expand(&providers.Provider{
		Roots: []providers.RootSpec{
			{Path: d},
			{Path: d + string(filepath.Separator)},
			{Glob: filepath.Join(base, "*")},
		},
	})
	if len(out) != 1 {
		t.Errorf("expected 1 deduplicated root, got %d (%v)", len(out), names(out))
	}
}

func TestSortChain(t *testing.T) {
	state := t.TempDir()
	m := NewManager(state)
	// recent.json puts "b" at the head of the LRU.
	raw, _ := json.Marshal([]string{"/roots/b"})
	if err := os.WriteFile(filepath.Join(state, "recent.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cands := []Candidate{
		{Root: "/roots/a", Recency: 300},
		{Root: "/roots/b", Recency: 100},
		{Root: "/roots/c", Recency: 200, HasOpenInstance: true},
		{Root: "/roots/d", Recency: 400, Label: "main"},
	}
	m.Sort(cands)

	got := []string{}
	for _, c := range cands {
		got = append(got, filepath.Base(c.Root))
	}
	// hasOpenInstance first, then the LRU hit, then recency desc.
	// "d" carries a label but still outranks "a" on recency: label must NOT sort last.
	want := []string{"c", "b", "d", "a"}
	if !slices.Equal(got, want) {
		t.Errorf("Sort() = %v, want %v", got, want)
	}
}

func TestTouchRecentIsAnLRUCappedAt20(t *testing.T) {
	state := t.TempDir()
	m := NewManager(state)
	for i := range 25 {
		m.TouchRecent(filepath.Join("/roots", string(rune('a'+i))))
	}
	got := m.Recent()
	if len(got) != 20 {
		t.Errorf("len(recent) = %d, want 20", len(got))
	}
	if got[0] != filepath.Join("/roots", string(rune('a'+24))) {
		t.Errorf("most recent entry = %q, want the last touched", got[0])
	}
	// Re-touching an existing entry moves it to the front without duplicating.
	m.TouchRecent(got[5])
	again := m.Recent()
	if again[0] != got[5] {
		t.Errorf("re-touched entry did not move to the front")
	}
	seen := map[string]int{}
	for _, p := range again {
		seen[p]++
		if seen[p] > 1 {
			t.Errorf("duplicate entry %q in the LRU", p)
		}
	}
}

func TestAllowedAndPathAllowed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "wt")
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	rs := []Root{{Path: root}}

	if _, ok := Allowed(rs, root); !ok {
		t.Error("the configured root must be allowed")
	}
	if _, ok := Allowed(rs, base); ok {
		t.Error("the parent of a root must NOT be allowed")
	}
	if _, ok := Allowed(rs, filepath.Join(root, "lib")); ok {
		t.Error("a subdirectory is not itself a spawn root")
	}
	if !PathAllowed(rs, filepath.Join(root, "lib", "x.go")) {
		t.Error("a path inside the root must be allowed")
	}
	if PathAllowed(rs, filepath.Join(base, "outside.txt")) {
		t.Error("a path outside every root must be rejected")
	}
	if PathAllowed(rs, "/etc/passwd") {
		t.Error("/etc/passwd must never be allowed")
	}
}
