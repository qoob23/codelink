package roots

import (
	"encoding/json"
	"fmt"
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

// repoFixture lays out two checkouts under a glob'ed directory plus one that no
// roots entry can ever enumerate, which is what repoAliases exists for.
func repoFixture(t *testing.T) (base string, glob providers.RootSpec) {
	t.Helper()
	base = t.TempDir()
	for _, d := range []string{"checkouts/synapses", "checkouts/codelink", "elsewhere/neurons"} {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return base, providers.RootSpec{Glob: filepath.Join(base, "checkouts", "*")}
}

func TestRepoScopeFilter(t *testing.T) {
	base, glob := repoFixture(t)
	outside := filepath.Join(base, "elsewhere", "neurons")
	m := NewManager(t.TempDir())

	tests := []struct {
		name    string
		repo    string
		aliases map[string]string
		want    []string
	}{
		{
			name: "no repo group means no filtering at all",
			repo: "", want: []string{"codelink", "synapses"},
		},
		{
			name: "only the checkout named after the repo survives",
			repo: "synapses", want: []string{"synapses"},
		},
		{
			name: "the repo name is matched case-insensitively",
			repo: "Synapses", want: []string{"synapses"},
		},
		{
			name: "an unknown repo leaves nothing to probe",
			repo: "ghost", want: nil,
		},
		{
			name: "an alias maps in a checkout named after something else",
			repo: "widgets", aliases: map[string]string{"widgets": filepath.Join(base, "checkouts", "codelink")},
			want: []string{"codelink"},
		},
		{
			name: "an alias target no roots entry enumerates is still eligible",
			repo: "widgets", aliases: map[string]string{"WIDGETS": outside},
			want: []string{"neurons"},
		},
		{
			name: "an alias adds to the name match rather than replacing it",
			repo: "synapses", aliases: map[string]string{"synapses": outside},
			want: []string{"neurons", "synapses"},
		},
		{
			name: "an alias pointing at the name match is not added twice",
			repo: "synapses", aliases: map[string]string{"synapses": filepath.Join(base, "checkouts", "synapses")},
			want: []string{"synapses"},
		},
		{
			name: "an alias to a missing directory is dropped",
			repo: "widgets", aliases: map[string]string{"widgets": filepath.Join(base, "nowhere")},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &providers.Provider{Roots: []providers.RootSpec{glob}, RepoAliases: tc.aliases}
			got := names(m.ScopeFor(p, tc.repo).Filter(m.Expand(p)))
			if !slices.Equal(got, tc.want) {
				t.Errorf("Filter() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRepoScopeAllows covers the open-instance side of the filter: an instance
// is judged by the directory it is rooted in, which may be a symlink to the
// alias target.
func TestRepoScopeAllows(t *testing.T) {
	base, _ := repoFixture(t)
	outside := filepath.Join(base, "elsewhere", "neurons")
	link := filepath.Join(base, "link-to-neurons")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.TempDir())
	p := &providers.Provider{RepoAliases: map[string]string{"synapses": outside}}

	tests := []struct {
		name string
		repo string
		dir  string
		want bool
	}{
		{name: "inert scope admits anything", repo: "", dir: "/anywhere", want: true},
		{name: "root named after the repo", repo: "synapses", dir: filepath.Join(base, "checkouts", "synapses"), want: true},
		{name: "trailing separator is cleaned away", repo: "synapses", dir: filepath.Join(base, "checkouts", "synapses") + "/", want: true},
		{name: "differently named root", repo: "synapses", dir: filepath.Join(base, "checkouts", "codelink"), want: false},
		{name: "case-insensitive name match", repo: "SYNAPSES", dir: filepath.Join(base, "checkouts", "synapses"), want: true},
		{name: "the alias target itself", repo: "synapses", dir: outside, want: true},
		{name: "a symlink to the alias target", repo: "synapses", dir: link, want: true},
		{name: "an alias configured for another repo", repo: "codelink", dir: outside, want: false},
		{name: "empty directory", repo: "synapses", dir: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.ScopeFor(p, tc.repo).Allows(tc.dir); got != tc.want {
				t.Errorf("Allows(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

// TestExpandAllIncludesAliasTargets is the allowlist half: /open builds its
// allowlist from ExpandAll, so an alias target the daemon offers as a candidate
// must also be spawnable.
func TestExpandAllIncludesAliasTargets(t *testing.T) {
	base, glob := repoFixture(t)
	outside := filepath.Join(base, "elsewhere", "neurons")
	m := NewManager(t.TempDir())
	cfg := &providers.Config{Providers: []*providers.Provider{{
		Roots:       []providers.RootSpec{glob},
		RepoAliases: map[string]string{"widgets": outside, "missing": filepath.Join(base, "nowhere")},
	}}}

	all := m.ExpandAll(cfg)
	if got, want := names(all), []string{"codelink", "neurons", "synapses"}; !slices.Equal(got, want) {
		t.Fatalf("ExpandAll() = %v, want %v", got, want)
	}
	if _, ok := Allowed(all, outside); !ok {
		t.Error("an alias target must be an allowed spawn root")
	}
	if !PathAllowed(all, filepath.Join(outside, "lib", "x.go")) {
		t.Error("a path inside an alias target must be allowed")
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

// TestProbeCacheIsBounded is the regression for a cache that only ever grew:
// its key embeds repoPath, which comes off the wire, so a page asking about a
// stream of unique paths could otherwise inflate the map for as long as the
// daemon runs.
func TestProbeCacheIsBounded(t *testing.T) {
	m := NewManager(t.TempDir())
	rs := []Root{{Path: t.TempDir()}}
	for i := range probeCap * 2 {
		m.Probe(rs, fmt.Sprintf("no/such/path/%d/x.go", i))
	}

	m.mu.RLock()
	n := len(m.probes)
	m.mu.RUnlock()

	if n > probeCap {
		t.Errorf("probe cache holds %d entries, want at most %d", n, probeCap)
	}
	if n == 0 {
		t.Error("probe cache is empty, so eviction threw away the whole point of it")
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
