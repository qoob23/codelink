package providers

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// testConfigJSON is a synthetic provider shaped like a real one: a wildcard
// host, several ordered path rules (a primary one, a legacy one sharing a
// prefix with non-file URLs, and a review/diff view whose file identity lives
// in the fragment), a provider-level fragment regex and a ref query parameter.
//
// It deliberately contains no real host, path or repository layout — those live
// only in the installed providers.json, which TestLoadInstalledConfigIfPresent
// validates structurally.
const testConfigJSON = `{
  "version": 1,
  "extensionId": "abcdefghijklmnopabcdefghijklmnop",
  "providers": [
    {
      "id": "example",
      "hosts": ["*.example.com"],
      "match": [
        { "path": "^/code/(?P<repoPath>.+)$" },
        { "path": "^/vcs/main/code/(?P<repoPath>.+)$" },
        {
          "path": "^/review/(?P<pr>\\d+)/files/\\d+/?$",
          "hash": "^file-(?P<repoPath>[^:]+?)(?::(?P<side>[LR])(?P<line>\\d+))?$"
        }
      ],
      "hash": "^L(?P<line>\\d+)(?:-L?(?P<endLine>\\d+))?(?::(?P<col>\\d+))?$",
      "refParam": "rev",
      "defaultRef": "main",
      "projectMarkers": ["lib", "test", "bin", "src", "example", "tool", "integration_test"],
      "roots": [
        { "glob": "~/checkouts/*", "requireMount": true, "recencyPath": "~/.local/state/checkouts/{name}" },
        { "path": "~/code", "label": "main" }
      ]
    }
  ]
}`

func loadTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(p, []byte(testConfigJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func ptr[T any](v T) *T { return &v }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

const fileURL = "https://a.example.com/code/owner/repo/pkg/widget/lib/src/main.go"
const fileRepoPath = "owner/repo/pkg/widget/lib/src/main.go"

func TestParse(t *testing.T) {
	cfg := loadTestConfig(t)

	tests := []struct {
		name    string
		url     string
		wantOK  bool
		path    string
		line    *int
		endLine *int
		col     *int
		side    *string
		ref     *string
		refDef  bool
	}{
		{
			name: "plain file, no fragment",
			url:  fileURL, wantOK: true, path: fileRepoPath, refDef: true,
		},
		{
			name: "#L12 single line",
			url:  fileURL + "#L12", wantOK: true, path: fileRepoPath,
			line: ptr(12), refDef: true,
		},
		{
			name: "#L12-20 bare range form (no second L)",
			url:  fileURL + "#L12-20", wantOK: true, path: fileRepoPath,
			line: ptr(12), endLine: ptr(20), refDef: true,
		},
		{
			name: "#L12-L20 repeated-L range form",
			url:  fileURL + "#L12-L20", wantOK: true, path: fileRepoPath,
			line: ptr(12), endLine: ptr(20), refDef: true,
		},
		{
			name: "#L0 is parsed as line 0 (nvim clamps, not the daemon)",
			url:  fileURL + "#L0", wantOK: true, path: fileRepoPath,
			line: ptr(0), refDef: true,
		},
		{
			name: "#L12:5 column",
			url:  fileURL + "#L12:5", wantOK: true, path: fileRepoPath,
			line: ptr(12), col: ptr(5), refDef: true,
		},
		{
			name: "?rev pinned revision",
			url:  fileURL + "?rev=r20488097#L12", wantOK: true, path: fileRepoPath,
			line: ptr(12), ref: ptr("r20488097"), refDef: false,
		},
		{
			name: "?rev=<defaultRef> is the default ref",
			url:  fileURL + "?rev=main", wantOK: true, path: fileRepoPath,
			ref: ptr("main"), refDef: true,
		},
		{
			name:   "review files view with side+line",
			url:    "https://a.example.com/review/123/files/4#file-owner/repo/x.go:R53",
			wantOK: true, path: "owner/repo/x.go",
			line: ptr(53), side: ptr("R"), refDef: true,
		},
		{
			name:   "review files view without a line",
			url:    "https://a.example.com/review/123/files/4#file-owner/repo/x.go",
			wantOK: true, path: "owner/repo/x.go", refDef: true,
		},
		{
			// Legacy but real file form; must not be confused with the review
			// URLs that share its /vcs/ prefix.
			name:   "legacy /vcs/main/code file form",
			url:    "https://a.example.com/vcs/main/code/owner/repo/pkg/types",
			wantOK: true, path: "owner/repo/pkg/types", refDef: true,
		},
		{
			name:   "legacy /vcs/main/code form with a line",
			url:    "https://a.example.com/vcs/main/code/owner/repo/pkg/types/x.h#L102-114",
			wantOK: true, path: "owner/repo/pkg/types/x.h",
			line: ptr(102), endLine: ptr(114), refDef: true,
		},
		{
			name:   "/vcs/review/... is a review URL, not a path prefix",
			url:    "https://a.example.com/vcs/review/123/details",
			wantOK: false,
		},
		{
			name:   "long-id /vcs/review/<N>/details must not parse as a file",
			url:    "https://a.example.com/vcs/review/10127110/details",
			wantOK: false,
		},
		{
			// Common case: no :R53 suffix, so the side/line group must
			// genuinely be optional or every plain diff link breaks.
			name:   "review diff without side suffix",
			url:    "https://a.example.com/review/10127110/files/1#file-owner/repo/pkg/net/lib/src/client.go",
			wantOK: true, path: "owner/repo/pkg/net/lib/src/client.go",
			refDef: true,
		},
		{
			name:   "review details page is not a file",
			url:    "https://a.example.com/review/123/details",
			wantOK: false,
		},
		{
			name:   "review files view with no fragment does not resolve a file",
			url:    "https://a.example.com/review/123/files/4",
			wantOK: false,
		},
		{
			name:   "unmatched host has no provider",
			url:    "https://code.other-host.org/code/owner/repo/x.go#L12",
			wantOK: false,
		},
		{
			name:   "lookalike host is not matched",
			url:    "https://evil-example.com/code/owner/x.go",
			wantOK: false,
		},
		{
			name:   "bare domain is covered by the *. glob",
			url:    "https://example.com/code/owner/x.go#L3",
			wantOK: true, path: "owner/x.go", line: ptr(3), refDef: true,
		},
		{
			name:   "deep subdomain is covered by the *. glob",
			url:    "https://a.b.example.com/code/owner/x.go",
			wantOK: true, path: "owner/x.go", refDef: true,
		},
		{
			name:   "directory URL with trailing slash",
			url:    "https://a.example.com/code/owner/repo/pkg/",
			wantOK: true, path: "owner/repo/pkg", refDef: true,
		},
		{
			name:   "path prefix alone does not resolve",
			url:    "https://a.example.com/code/",
			wantOK: false,
		},
		{
			name:   "percent-encoded path segment is decoded",
			url:    "https://a.example.com/code/owner/my%20dir/x.go",
			wantOK: true, path: "owner/my dir/x.go", refDef: true,
		},
		{
			name:   "unparseable fragment is ignored, file still resolves",
			url:    fileURL + "#some-anchor",
			wantOK: true, path: fileRepoPath, refDef: true,
		},
		{
			name:   "plain http is accepted",
			url:    "http://a.example.com/code/owner/x.go",
			wantOK: true, path: "owner/x.go", refDef: true,
		},
		{
			// url.Parse lower-cases the scheme, so the check must not compare
			// against the raw URL text.
			name:   "upper-case scheme is still http",
			url:    "HTTPS://a.example.com/code/owner/x.go",
			wantOK: true, path: "owner/x.go", refDef: true,
		},
		{
			// url.Parse hands each of these a Host, so with no scheme check
			// they match a provider on the host alone.
			name:   "javascript: URL is not a code host",
			url:    "javascript://a.example.com/code/owner/x.go",
			wantOK: false,
		},
		{
			name:   "file: URL is not a code host",
			url:    "file://a.example.com/code/owner/x.go",
			wantOK: false,
		},
		{
			name:   "ftp: URL is not a code host",
			url:    "ftp://a.example.com/code/owner/x.go",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cfg.Parse(tc.url)
			if ok != tc.wantOK {
				t.Fatalf("Parse(%q) ok=%v want %v (got %+v)", tc.url, ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.Provider != "example" {
				t.Errorf("provider = %q, want example", got.Provider)
			}
			if got.RepoPath != tc.path {
				t.Errorf("repoPath = %q, want %q", got.RepoPath, tc.path)
			}
			if !eqIntPtr(got.Line, tc.line) {
				t.Errorf("line = %v, want %v", deref(got.Line), deref(tc.line))
			}
			if !eqIntPtr(got.EndLine, tc.endLine) {
				t.Errorf("endLine = %v, want %v", deref(got.EndLine), deref(tc.endLine))
			}
			if !eqIntPtr(got.Col, tc.col) {
				t.Errorf("col = %v, want %v", deref(got.Col), deref(tc.col))
			}
			if !eqStrPtr(got.Side, tc.side) {
				t.Errorf("side = %v, want %v", got.Side, tc.side)
			}
			if !eqStrPtr(got.Ref, tc.ref) {
				t.Errorf("ref = %v, want %v", got.Ref, tc.ref)
			}
			if got.RefIsDefault != tc.refDef {
				t.Errorf("refIsDefault = %v, want %v", got.RefIsDefault, tc.refDef)
			}
		})
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestHostMatches(t *testing.T) {
	tests := []struct {
		pattern, host string
		want          bool
	}{
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "example.com", true},
		{"*.example.com", "x.y.example.com", true},
		{"*.example.com", "evil-example.com", false},
		{"*.example.com", "example.com.evil.net", false},
		{"*.example.com", "other-host.org", false},
		{"a.example.com", "a.example.com", true},
		{"a.example.com", "b.example.com", false},
		{"*.example.com", "A.Example.COM", true},
		{"", "a.example.com", false},
	}
	for _, tc := range tests {
		if got := HostMatches(tc.pattern, tc.host); got != tc.want {
			t.Errorf("HostMatches(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}

func TestMatchPatterns(t *testing.T) {
	cfg := loadTestConfig(t)
	got := cfg.AllMatchPatterns()
	want := []string{"*://*.example.com/*"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("AllMatchPatterns() = %v, want %v", got, want)
	}
	hosts := cfg.AllHosts()
	if len(hosts) != 1 || hosts[0] != "*.example.com" {
		t.Errorf("AllHosts() = %v, want [*.example.com]", hosts)
	}
	if cfg.ExtensionID != "abcdefghijklmnopabcdefghijklmnop" {
		t.Errorf("extensionId = %q", cfg.ExtensionID)
	}
}

func TestLoadRejectsBadRegex(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	bad := `{"version":1,"providers":[{"id":"x","hosts":["a.com"],"match":[{"path":"^/(?P<repoPath>["}]}]}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected Load to reject an uncompilable regex")
	}
}

// declaresRepoPath reports whether a match entry can ever yield a repoPath,
// either from its own path/hash regex or from the provider-level fallback.
func declaresRepoPath(p *Provider, m *MatchEntry) bool {
	for _, re := range []*regexp.Regexp{m.pathRe, m.hashRe, p.hashRe} {
		if re != nil && re.SubexpIndex("repoPath") >= 0 {
			return true
		}
	}
	return false
}

// TestLoadInstalledConfigIfPresent checks the config actually installed on this
// machine, without knowing anything about its contents: it must load, compile
// and satisfy the invariants every consumer relies on. Set $CODELINK_TEST_URL
// to additionally require that a specific URL round-trips through it.
func TestLoadInstalledConfigIfPresent(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p := filepath.Join(home, ".local", "share", "codelink", "providers.json")
	if _, err := os.Stat(p); err != nil {
		t.Skip("no providers.json installed")
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load(installed providers.json): %v", err)
	}
	if cfg.ExtensionID == "" {
		t.Error("extensionId is empty, so no origin would ever be accepted")
	}
	if len(cfg.AllHosts()) == 0 || len(cfg.AllMatchPatterns()) == 0 {
		t.Error("no hosts declared, so the extension would match nothing")
	}
	for _, prov := range cfg.Providers {
		if len(prov.Hosts) == 0 {
			t.Errorf("provider %s declares no hosts", prov.ID)
		}
		if len(prov.Match) == 0 {
			t.Errorf("provider %s declares no match rules", prov.ID)
		}
		for i := range prov.Match {
			if !declaresRepoPath(prov, &prov.Match[i]) {
				t.Errorf("provider %s: match[%d] can never yield a repoPath", prov.ID, i)
			}
		}
		if len(prov.Roots) == 0 {
			t.Errorf("provider %s declares no roots, so nothing could resolve locally", prov.ID)
		}
	}
	if u := os.Getenv("CODELINK_TEST_URL"); u != "" {
		parsed, ok := cfg.Parse(u)
		if !ok {
			t.Fatalf("installed config failed to parse $CODELINK_TEST_URL %q", u)
		}
		if parsed.RepoPath == "" {
			t.Errorf("installed config parsed %q to an empty repoPath: %+v", u, parsed)
		}
	}
}
