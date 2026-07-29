// Package roots enumerates the local checkouts a repo path may live in and
// probes them for that path.
//
// Two properties of such checkouts drive the design:
//
//   - A root may be a network- or FUSE-backed mount rather than plain local
//     disk. A cold stat there costs 440-510ms against ~20ms warm, and a stat
//     against a STALE (no longer mounted) directory pays the full cold price to
//     learn nothing. Hence the mount(8) filter, the per-probe deadline and the
//     TTL caches.
//   - Some checkout backends report an IDENTICAL mtime for every file they
//     serve — the time the mount came up — which makes file mtime useless for
//     ranking checkouts against each other. Real recency must therefore be read
//     from a path on local disk, which each root supplies as recencyPath.
package roots

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"codelink/internal/providers"
	"codelink/internal/resolve"
)

const (
	mountTTL    = 30 * time.Second
	positiveTTL = 60 * time.Second
	negativeTTL = 10 * time.Second
	probeBudget = 400 * time.Millisecond
	recentCap   = 20

	// probeCap bounds the probe cache. Its key embeds repoPath, which arrives
	// straight off the wire, so without a ceiling a page that asks about a
	// stream of unique paths grows the map until the daemon is killed — and the
	// only other eviction, the flush in mounts(), fires just when the mount set
	// changes. The cache exists to spare the handful of paths a human actually
	// hovers a cold ~500ms stat, and that working set is tiny, so the cap is
	// far above any real usage and the crude eviction below never runs in
	// practice.
	probeCap = 1024
)

// Root is one expanded local checkout.
type Root struct {
	Path         string    `json:"root"`
	Label        string    `json:"label,omitempty"`
	Recency      int64     `json:"recency"`
	RecencyTime  time.Time `json:"-"`
	RequireMount bool      `json:"-"`
	Mounted      bool      `json:"-"`
}

// Name is the basename used for the {name} placeholder in recencyPath.
func (r Root) Name() string { return filepath.Base(r.Path) }

// Candidate is a root that actually contains the requested repo path.
type Candidate struct {
	Root            string `json:"root"`
	Label           string `json:"label,omitempty"`
	LocalPath       string `json:"localPath"`
	Recency         int64  `json:"recency"`
	HasOpenInstance bool   `json:"hasOpenInstance"`
}

type probeResult struct {
	localPath string
	ok        bool
	at        time.Time
}

// Manager owns the mount set, probe cache and recent.json LRU.
type Manager struct {
	stateDir string

	mu     sync.RWMutex
	probes map[string]probeResult

	mountMu   sync.Mutex
	mountSet  map[string]bool
	mountKey  string
	mountedAt time.Time
}

// NewManager returns a manager storing recent.json under stateDir.
func NewManager(stateDir string) *Manager {
	return &Manager{stateDir: stateDir, probes: map[string]probeResult{}}
}

// ExpandTilde replaces a leading ~ with the user's home directory.
func ExpandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// isDir reports whether path is a directory.
//
// os.Stat, not Lstat: a worktree may legitimately be a symlink to a directory,
// and Lstat would silently drop it. Both the "path" and "glob" branches of
// Expand go through here so they cannot drift apart.
//
// Cost note: for a stale FUSE mountpoint this is still cheap, because the
// mountpoint itself is a plain local directory once the filesystem is gone —
// the expensive stats are the ones that descend INTO a live mount.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Expand turns a provider's root specs into concrete directories, dropping
// unmounted ones where requireMount is set and filling in recency.
func (m *Manager) Expand(p *providers.Provider) []Root {
	var out []Root
	seen := map[string]bool{}

	add := func(path string, spec providers.RootSpec) {
		path = filepath.Clean(ExpandTilde(path))
		if seen[path] {
			return
		}
		if !isDir(path) {
			return
		}
		seen[path] = true
		label := spec.Label
		if label == "" {
			// Glob-expanded roots carry no label of their own. Default it here
			// rather than leaving the field absent, so every consumer sees the
			// same contract and none has to reinvent the fallback.
			label = filepath.Base(path)
		}
		out = append(out, Root{
			Path:         path,
			Label:        label,
			RequireMount: spec.RequireMount,
		})
	}

	for _, spec := range p.Roots {
		switch {
		case spec.Glob != "":
			matches, err := filepath.Glob(ExpandTilde(spec.Glob))
			if err != nil {
				continue
			}
			sort.Strings(matches)
			for _, mpath := range matches {
				base := filepath.Base(mpath)
				// Skip dotfiles like .envrc / .DS_Store that the glob picks up.
				if strings.HasPrefix(base, ".") {
					continue
				}
				add(mpath, spec)
			}
		case spec.Path != "":
			add(spec.Path, spec)
		}
	}

	mounts := m.mounts()
	filtered := out[:0]
	for _, r := range out {
		r.Mounted = mounts[filepath.Clean(r.Path)]
		if r.RequireMount && !r.Mounted {
			// A checkout directory outlives its filesystem: once unmounted the
			// mountpoint is still a perfectly ordinary directory, so isDir
			// cannot tell it apart from a live root. Probing into one costs a
			// full ~500ms cold stat for a guaranteed miss, so drop it here.
			continue
		}
		filtered = append(filtered, r)
	}
	out = filtered

	m.fillRecency(out, p.Roots)
	return out
}

// ExpandAll enumerates the roots of every provider, repoAliases targets
// included.
//
// SECURITY: this list is the /open allowlist. An alias target is declared in
// providers.json just like a root and is probed like one, so leaving it out
// would make the daemon refuse to spawn in a checkout it just offered.
func (m *Manager) ExpandAll(cfg *providers.Config) []Root {
	var out []Root
	seen := map[string]bool{}
	for _, p := range cfg.Providers {
		for _, r := range append(m.Expand(p), m.aliasRoots(p)...) {
			if !seen[r.Path] {
				seen[r.Path] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// aliasRoot expands one repoAliases target into a root, filling in recency from
// the provider's specs so it ranks alongside the enumerated ones.
func (m *Manager) aliasRoot(p *providers.Provider, target string) (Root, bool) {
	path := filepath.Clean(ExpandTilde(target))
	if !isDir(path) {
		return Root{}, false
	}
	rs := []Root{{Path: path, Label: filepath.Base(path)}}
	m.fillRecency(rs, p.Roots)
	return rs[0], true
}

// aliasRoots expands every repoAliases target of a provider. The targets are
// sorted first so the result does not depend on map iteration order.
func (m *Manager) aliasRoots(p *providers.Provider) []Root {
	targets := make([]string, 0, len(p.RepoAliases))
	for _, t := range p.RepoAliases {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	var out []Root
	seen := map[string]bool{}
	for _, t := range targets {
		r, ok := m.aliasRoot(p, t)
		if !ok || seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		out = append(out, r)
	}
	return out
}

// RepoScope restricts a resolve to the checkout of one repository, using the
// repo name the provider's "repo" group captured.
//
// The same file path exists in many checkouts, so without this a link to repo A
// happily opens the same-named file in checkout B — including through an nvim
// that is already running there.
//
// The zero value (empty repo) admits everything, which is what a provider whose
// regexes capture no repo group must keep getting.
type RepoScope struct {
	repo     string
	alias    Root
	hasAlias bool
}

// ScopeFor resolves the repoAliases entry once, so the root filter and the
// open-instance filter judge against the same directory.
func (m *Manager) ScopeFor(p *providers.Provider, repo string) RepoScope {
	sc := RepoScope{repo: repo}
	if repo == "" {
		return sc
	}
	if target := p.RepoAlias(repo); target != "" {
		sc.alias, sc.hasAlias = m.aliasRoot(p, target)
	}
	return sc
}

// Active reports whether the scope excludes anything at all.
func (sc RepoScope) Active() bool { return sc.repo != "" }

// Allows reports whether dir may serve the scoped repo: it is named after the
// repository, or it IS the checkout repoAliases points at. The alias comparison
// resolves symlinks on both sides, like every other directory-identity test
// here — a checkout root is routinely reached through one.
func (sc RepoScope) Allows(dir string) bool {
	if !sc.Active() {
		return true
	}
	if dir == "" {
		return false
	}
	dir = filepath.Clean(ExpandTilde(dir))
	if strings.EqualFold(filepath.Base(dir), sc.repo) {
		return true
	}
	return sc.hasAlias && resolve.SameDir(dir, sc.alias.Path)
}

// Filter keeps the eligible roots and adds the alias target, which is a root in
// its own right even when no roots entry enumerates it.
//
// This runs BEFORE Probe on purpose: an ineligible root must not be stat'ed at
// all — its answer would be wrong, and on a cold mount it costs ~500ms to
// produce.
func (sc RepoScope) Filter(rs []Root) []Root {
	if !sc.Active() {
		return rs
	}
	out := make([]Root, 0, len(rs)+1)
	for _, r := range rs {
		if sc.Allows(r.Path) {
			out = append(out, r)
		}
	}
	if sc.hasAlias && !slices.ContainsFunc(out, func(r Root) bool { return resolve.SameDir(r.Path, sc.alias.Path) }) {
		out = append(out, sc.alias)
	}
	return out
}

// fillRecency resolves each root's recencyPath template and stats it. The
// template points at real local disk, so this is fast and — unlike the mtime of
// anything served by the mount — actually varies per checkout. Roots with no
// usable recencyPath fall back to the mtime of the root directory itself.
func (m *Manager) fillRecency(rs []Root, specs []providers.RootSpec) {
	tmpl := ""
	for _, s := range specs {
		if s.RecencyPath != "" {
			tmpl = s.RecencyPath
			break
		}
	}
	for i := range rs {
		r := &rs[i]
		if tmpl != "" {
			p := ExpandTilde(strings.ReplaceAll(tmpl, "{name}", r.Name()))
			if fi, err := os.Stat(p); err == nil {
				r.RecencyTime = fi.ModTime()
				r.Recency = fi.ModTime().Unix()
				continue
			}
		}
		if fi, err := os.Stat(r.Path); err == nil {
			r.RecencyTime = fi.ModTime()
			r.Recency = fi.ModTime().Unix()
		}
	}
}

// mounts returns the set of currently mounted directories, cached for 30s.
// When the set changes the probe cache is flushed, because a freshly mounted
// root turns every cached negative into a lie.
func (m *Manager) mounts() map[string]bool {
	m.mountMu.Lock()
	defer m.mountMu.Unlock()
	if m.mountSet != nil && time.Since(m.mountedAt) < mountTTL {
		return m.mountSet
	}
	set, key := readMounts()
	if m.mountSet != nil && key != m.mountKey {
		m.mu.Lock()
		m.probes = map[string]probeResult{}
		m.mu.Unlock()
	}
	m.mountSet, m.mountKey, m.mountedAt = set, key, time.Now()
	return set
}

// readMounts parses mount(8): "<src> on <dir> (<opts>)".
func readMounts() (map[string]bool, string) {
	set := map[string]bool{}
	var keys []string
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/sbin/mount").Output()
	if err != nil {
		return set, ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, " on ")
		if i < 0 {
			continue
		}
		rest := line[i+4:]
		if j := strings.LastIndex(rest, " ("); j >= 0 {
			rest = rest[:j]
		}
		dir := filepath.Clean(strings.TrimSpace(rest))
		if dir == "" {
			continue
		}
		set[dir] = true
		keys = append(keys, dir)
	}
	sort.Strings(keys)
	return set, strings.Join(keys, "\x00")
}

// Probe resolves repoPath inside every root concurrently, honouring the cache.
func (m *Manager) Probe(rs []Root, repoPath string) []Candidate {
	type res struct {
		root Root
		pr   probeResult
	}
	ch := make(chan res, len(rs))
	var wg sync.WaitGroup
	for _, r := range rs {
		if pr, ok := m.cached(r.Path, repoPath); ok {
			ch <- res{r, pr}
			continue
		}
		wg.Add(1)
		go func(r Root) {
			defer wg.Done()
			ch <- res{r, m.probeOne(r.Path, repoPath)}
		}(r)
	}
	wg.Wait()
	close(ch)

	var out []Candidate
	for r := range ch {
		if !r.pr.ok {
			continue
		}
		out = append(out, Candidate{
			Root:      r.root.Path,
			Label:     r.root.Label,
			LocalPath: r.pr.localPath,
			Recency:   r.root.Recency,
		})
	}
	return out
}

func cacheKey(root, repoPath string) string { return root + "\x00" + repoPath }

// expired applies the asymmetric TTL: a miss is re-checked ten times sooner
// than a hit, because a checkout gains files far more often than it loses them.
func expired(pr probeResult, now time.Time) bool {
	ttl := negativeTTL
	if pr.ok {
		ttl = positiveTTL
	}
	return now.Sub(pr.at) > ttl
}

func (m *Manager) cached(root, repoPath string) (probeResult, bool) {
	key := cacheKey(root, repoPath)
	m.mu.RLock()
	pr, ok := m.probes[key]
	m.mu.RUnlock()
	if !ok {
		return probeResult{}, false
	}
	if expired(pr, time.Now()) {
		// Drop it here rather than leaving it for the eventual re-probe to
		// overwrite: an entry nobody looks up again is dead weight either way,
		// but while it sits in the map it occupies one of probeCap's slots and
		// pushes a live entry towards eviction.
		m.mu.Lock()
		if cur, still := m.probes[key]; still && expired(cur, time.Now()) {
			delete(m.probes, key)
		}
		m.mu.Unlock()
		return probeResult{}, false
	}
	return pr, true
}

// store records a probe result, making room first when the cache is full.
func (m *Manager) store(key string, pr probeResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.probes[key]; !ok && len(m.probes) >= probeCap {
		m.evictLocked()
	}
	m.probes[key] = pr
}

// evictLocked frees a slot: expired entries first, then arbitrary ones. Go
// randomises map iteration, and "arbitrary" is good enough here — evicting a
// still-warm entry costs one re-probe, never a wrong answer.
func (m *Manager) evictLocked() {
	now := time.Now()
	for k, pr := range m.probes {
		if expired(pr, now) {
			delete(m.probes, k)
		}
	}
	for k := range m.probes {
		if len(m.probes) < probeCap {
			return
		}
		delete(m.probes, k)
	}
}

// probeOne runs the resolve against one root under a hard time budget. os.Stat
// is not cancellable, so the goroutine may outlive the context; the buffered
// channel keeps that leak bounded and short-lived.
func (m *Manager) probeOne(root, repoPath string) probeResult {
	ctx, cancel := context.WithTimeout(context.Background(), probeBudget)
	defer cancel()

	done := make(chan probeResult, 1)
	go func() {
		local, _, ok := resolve.Resolve(repoPath, root)
		done <- probeResult{localPath: local, ok: ok, at: time.Now()}
	}()

	var pr probeResult
	select {
	case pr = <-done:
	case <-ctx.Done():
		// Timed out: record a short-lived negative so a slow cold mount does
		// not stall every subsequent hover, but re-probe again soon.
		pr = probeResult{ok: false, at: time.Now()}
	}
	m.store(cacheKey(root, repoPath), pr)
	return pr
}

// Warm issues one probe per root in the background so the first real hover
// lands on an already-warm mount rather than paying the ~500ms cold price.
func (m *Manager) Warm(rs []Root) {
	for _, r := range rs {
		go func(path string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan struct{}, 1)
			go func() {
				_, _ = os.Stat(path)
				done <- struct{}{}
			}()
			select {
			case <-done:
			case <-ctx.Done():
			}
		}(r.Path)
	}
}

// Sort orders the candidates most-relevant-first:
//
//	hasOpenInstance desc -> recent.json LRU position -> recency desc
//
// Label plays no part: it is purely a display name.
func (m *Manager) Sort(cands []Candidate) {
	lru := m.Recent()
	pos := map[string]int{}
	for i, p := range lru {
		pos[filepath.Clean(p)] = i
	}
	rank := func(c Candidate) int {
		if i, ok := pos[filepath.Clean(c.Root)]; ok {
			return i
		}
		return len(lru) + 1
	}
	sort.SliceStable(cands, func(a, b int) bool {
		x, y := cands[a], cands[b]
		if x.HasOpenInstance != y.HasOpenInstance {
			return x.HasOpenInstance
		}
		if rx, ry := rank(x), rank(y); rx != ry {
			return rx < ry
		}
		if x.Recency != y.Recency {
			return x.Recency > y.Recency
		}
		return x.Root < y.Root
	})
}

func (m *Manager) recentPath() string { return filepath.Join(m.stateDir, "recent.json") }

// Recent returns the LRU list of roots opened through codelink.
func (m *Manager) Recent() []string {
	raw, err := os.ReadFile(m.recentPath())
	if err != nil {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// TouchRecent moves root to the front of the LRU, capped at 20 entries.
func (m *Manager) TouchRecent(root string) {
	root = filepath.Clean(root)
	list := m.Recent()
	out := []string{root}
	for _, p := range list {
		if filepath.Clean(p) != root {
			out = append(out, p)
		}
	}
	if len(out) > recentCap {
		out = out[:recentCap]
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(m.recentPath(), append(raw, '\n'), 0o644)
}

// Allowed reports whether dir is one of the enumerated roots, comparing
// physical paths on both sides.
//
// SECURITY CONTROL, not tidiness: a user's nvim config may set opt.exrc = true,
// so the cwd nvim is launched in is searched for a project-local .nvim.lua
// (0.12 also walks its parents). Since 0.9 nvim sources one only if it is on
// the |trust| list, so an attacker-chosen cwd is not code execution on its own;
// what it decides is which trusted config runs and where the user's editor is
// rooted. Only directories the daemon itself enumerated from providers.json may
// ever be used as a spawn target.
func Allowed(rs []Root, dir string) (Root, bool) {
	want := resolve.Canonical(dir)
	for _, r := range rs {
		if resolve.Canonical(r.Path) == want {
			return r, true
		}
	}
	return Root{}, false
}

// PathAllowed reports whether a resolved local path lies inside one of the
// enumerated roots. Clean/EvalSymlinks on both sides kills ../ traversal and
// symlink escapes.
func PathAllowed(rs []Root, p string) bool {
	cp := resolve.Canonical(p)
	for _, r := range rs {
		if resolve.Within(resolve.Canonical(r.Path), cp) {
			return true
		}
	}
	return false
}
