// Package providers loads providers.json and turns a code-hosting URL into a
// repo-relative path plus line/range/side/ref metadata.
//
// All regexes are Go's RE2: (?P<name>…) named groups, no backreferences and no
// lookaround. Groups are read back via (*regexp.Regexp).SubexpIndex.
package providers

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// DefaultConfigPath is used when $CODELINK_PROVIDERS is unset.
const DefaultConfigPath = "~/.local/share/codelink/providers.json"

// RootSpec is one entry of a provider's "roots" list. Exactly one of Path or
// Glob is expected to be set.
type RootSpec struct {
	Path         string `json:"path"`
	Glob         string `json:"glob"`
	Label        string `json:"label"`
	RequireMount bool   `json:"requireMount"`
	RecencyPath  string `json:"recencyPath"`
}

// MatchEntry is one ordered {path, hash?} rule.
type MatchEntry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`

	pathRe *regexp.Regexp
	hashRe *regexp.Regexp
}

// Provider describes one code host.
type Provider struct {
	ID             string       `json:"id"`
	Hosts          []string     `json:"hosts"`
	Match          []MatchEntry `json:"match"`
	Hash           string       `json:"hash"`
	RefParam       string       `json:"refParam"`
	DefaultRef     string       `json:"defaultRef"`
	ProjectMarkers []string     `json:"projectMarkers"`
	Roots          []RootSpec   `json:"roots"`

	// RepoAliases maps a repo name — as captured by the "repo" group — onto the
	// local checkout serving it, for the checkouts whose directory is not named
	// after the repository. Targets are enumerated from providers.json exactly
	// like roots, so they carry the same standing.
	RepoAliases map[string]string `json:"repoAliases"`

	hashRe *regexp.Regexp
}

// Config is the whole providers.json document.
type Config struct {
	Version     int         `json:"version"`
	ExtensionID string      `json:"extensionId"`
	Providers   []*Provider `json:"providers"`

	// Inject overrides which PAGES the content script is injected into.
	// Optional; empty means <all_urls>.
	//
	// Not to be confused with which LINKS get a button — that is decided per
	// link from the provider hosts. Injecting everywhere is what lets a button
	// appear on a page that merely *mentions* a repo link (a local HTML report,
	// a ticket, a chat log), none of which live on a provider host.
	Inject []string `json:"inject"`

	// Source is the file the config was read from.
	Source string `json:"-"`
}

// Parsed is what a URL decomposes into.
type Parsed struct {
	Provider string `json:"provider"`
	// Repo is the repository name, when the provider captures one. Absent means
	// the URL says nothing about which checkout it belongs to — and omitempty
	// keeps the payload of a provider that captures no repo group byte-identical
	// to what it was before the field existed.
	Repo         string  `json:"repo,omitempty"`
	RepoPath     string  `json:"repoPath"`
	Line         *int    `json:"line"`
	EndLine      *int    `json:"endLine"`
	Col          *int    `json:"col"`
	Side         *string `json:"side"`
	Ref          *string `json:"ref"`
	RefIsDefault bool    `json:"refIsDefault"`
	Project      string  `json:"project"`
	ProjectName  string  `json:"projectName"`
	Kind         string  `json:"kind"`

	// provider is the provider that produced this parse; not serialised.
	provider *Provider
}

// Owner returns the provider that matched the URL.
func (p *Parsed) Owner() *Provider { return p.provider }

// Load reads and compiles a providers.json.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	// Unknown fields are tolerated on purpose: providers.json is also read by
	// the browser extension and may gain keys the daemon does not care about.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Source = path
	if err := cfg.compile(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) compile() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers declared")
	}
	for _, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("provider with empty id")
		}
		if err := p.checkRepoAliases(); err != nil {
			return err
		}
		if p.Hash != "" {
			re, err := regexp.Compile(p.Hash)
			if err != nil {
				return fmt.Errorf("provider %s: hash: %w", p.ID, err)
			}
			p.hashRe = re
		}
		for i := range p.Match {
			m := &p.Match[i]
			re, err := regexp.Compile(m.Path)
			if err != nil {
				return fmt.Errorf("provider %s: match[%d].path: %w", p.ID, i, err)
			}
			m.pathRe = re
			if m.Hash != "" {
				hre, err := regexp.Compile(m.Hash)
				if err != nil {
					return fmt.Errorf("provider %s: match[%d].hash: %w", p.ID, i, err)
				}
				m.hashRe = hre
			}
		}
	}
	return nil
}

// checkRepoAliases rejects entries no consumer could act on.
//
// Duplicate names are an error rather than a last-one-wins because RepoAlias
// matches case-insensitively: two names differing only in case would otherwise
// make the answer depend on Go's randomised map iteration order.
func (p *Provider) checkRepoAliases() error {
	seen := make(map[string]bool, len(p.RepoAliases))
	for name, target := range p.RepoAliases {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("provider %s: repoAliases has an entry with an empty repo name", p.ID)
		}
		if strings.TrimSpace(target) == "" {
			return fmt.Errorf("provider %s: repoAliases[%q]: empty path", p.ID, name)
		}
		key := foldRepo(name)
		if seen[key] {
			return fmt.Errorf("provider %s: repoAliases declares %q twice (names are case-insensitive)", p.ID, key)
		}
		seen[key] = true
	}
	return nil
}

// foldRepo is the single case fold repo names are compared under. Validation and
// lookup must agree on it: strings.EqualFold and strings.ToLower disagree on
// exotic folds (U+017F ſ folds to "s" for one and not the other), which would
// let two names the check accepted as distinct both answer to one lookup.
func foldRepo(name string) string { return strings.ToLower(name) }

// RepoAlias returns the checkout configured for a repo name, or "" when none is.
// The name arrives verbatim from a URL, so the comparison is case-insensitive.
func (p *Provider) RepoAlias(repo string) string {
	if repo == "" {
		return ""
	}
	want := foldRepo(repo)
	for name, target := range p.RepoAliases {
		if foldRepo(name) == want {
			return target
		}
	}
	return ""
}

// HostMatches reports whether host is covered by pattern.
//
// A leading "*." matches any subdomain AND the bare domain, so
// "*.example.com" covers "a.example.com", "x.y.example.com" and "example.com"
// itself. A hostname that merely ends in the pattern without a dot boundary
// (e.g. "notexample.com") is deliberately NOT matched.
func HostMatches(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = strings.ToLower(strings.TrimSpace(host))
	if pattern == "" || host == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		bare := pattern[2:]
		return host == bare || strings.HasSuffix(host, "."+bare)
	}
	return host == pattern
}

// ForHost returns the first provider declaring a host pattern covering host.
func (c *Config) ForHost(host string) *Provider {
	for _, p := range c.Providers {
		for _, h := range p.Hosts {
			if HostMatches(h, host) {
				return p
			}
		}
	}
	return nil
}

// Parse decomposes a full URL. ok is false when no provider claims the host or
// when no match entry accepts the path.
func (c *Config) Parse(rawURL string) (*Parsed, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return nil, false
	}
	// url.Parse is happy to give "javascript://code.example.com/repo/x" a host,
	// and so is "file://". Matching those on the host alone would let a link the
	// browser would never navigate as a code host drive a resolve, and the
	// daemon does not delegate that judgement to the content script. Parse
	// lower-cases the scheme, so a literal comparison is enough.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}
	p := c.ForHost(u.Hostname())
	if p == nil {
		return nil, false
	}
	return p.parseURL(u)
}

func (p *Provider) parseURL(u *url.URL) (*Parsed, bool) {
	urlPath := u.Path
	if urlPath == "" {
		urlPath = "/"
	}
	frag := u.Fragment

	for i := range p.Match {
		m := &p.Match[i]
		pm := m.pathRe.FindStringSubmatch(urlPath)
		if pm == nil {
			continue
		}
		// An entry carrying its own hash regex only wins if the fragment also
		// matches it; otherwise fall through to the next entry.
		var hm []string
		hashRe := m.hashRe
		if hashRe != nil {
			hm = hashRe.FindStringSubmatch(frag)
			if hm == nil {
				continue
			}
		} else if p.hashRe != nil {
			// The provider-level fallback fragment regex is optional: a URL
			// without a usable fragment still resolves to the file.
			hm = p.hashRe.FindStringSubmatch(frag)
			hashRe = p.hashRe
		}

		out := &Parsed{Provider: p.ID, provider: p}
		get := func(name string) (string, bool) {
			if v, ok := group(m.pathRe, pm, name); ok {
				return v, true
			}
			if hashRe != nil && hm != nil {
				return group(hashRe, hm, name)
			}
			return "", false
		}

		repoPath, ok := get("repoPath")
		if !ok || repoPath == "" {
			continue
		}
		out.RepoPath = strings.TrimPrefix(strings.TrimSuffix(repoPath, "/"), "/")
		if out.RepoPath == "" {
			continue
		}
		// repo is optional: a provider that captures none keeps resolving against
		// every configured root.
		out.Repo, _ = get("repo")
		out.Line = intGroup(get, "line")
		out.EndLine = intGroup(get, "endLine")
		out.Col = intGroup(get, "col")
		if v, ok := get("side"); ok && v != "" {
			out.Side = &v
		}

		out.RefIsDefault = true
		if p.RefParam != "" {
			if rev := u.Query().Get(p.RefParam); rev != "" {
				r := rev
				out.Ref = &r
				out.RefIsDefault = rev == p.DefaultRef
			}
		}
		return out, true
	}
	return nil, false
}

func group(re *regexp.Regexp, m []string, name string) (string, bool) {
	idx := re.SubexpIndex(name)
	if idx < 0 || idx >= len(m) || m[idx] == "" {
		return "", false
	}
	return m[idx], true
}

func intGroup(get func(string) (string, bool), name string) *int {
	v, ok := get(name)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// MatchPatterns expands the provider host globs into Chromium match patterns,
// e.g. "*.example.com" -> "*://*.example.com/*". Chromium's own "*." wildcard
// already covers the bare domain, so no second pattern is needed.
func (p *Provider) MatchPatterns() []string {
	out := make([]string, 0, len(p.Hosts))
	for _, h := range p.Hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		out = append(out, "*://"+h+"/*")
	}
	return out
}

// AllMatchPatterns is the de-duplicated union over every provider.
func (c *Config) AllMatchPatterns() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Providers {
		for _, m := range p.MatchPatterns() {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// AllHosts is the de-duplicated union of raw host globs, for the content
// script's cheap triage.
func (c *Config) AllHosts() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range c.Providers {
		for _, h := range p.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}
