package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"codelink/internal/resolve"
	"codelink/internal/roots"
)

// fakeNvim writes a stub standing in for `nvim --server ... --remote-expr`.
// The real binary prints the RPC's JSON string result on stdout and exits 0,
// so the stub does the same. It records its argv for inspection.
func fakeNvim(t *testing.T, dir, body string) (bin string, logPath string) {
	t.Helper()
	bin = filepath.Join(dir, "fake-nvim")
	logPath = filepath.Join(dir, "nvim-args.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
printf '%%s' %q
`, logPath, body)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

// testPort is the port the test servers claim to listen on. It has to be a real
// value rather than 0 because the guard now matches the Host header against it.
const testPort = 47391

// testServer wires a Server against throwaway dirs, with allowedRoot as the one
// configured root.
func testServer(t *testing.T, allowedRoot, nvimBin string) *Server {
	t.Helper()
	return testServerWith(t, fmt.Sprintf(`{
      "version":1,"extensionId":"testext",
      "providers":[{"id":"t","hosts":["*.example.com"],
        "match":[{"path":"^/src/(?P<repoPath>.+)$"}],
        "hash":"^L(?P<line>\\d+)$",
        "projectMarkers":["lib"],
        "roots":[{"path":%q}]}]}`, allowedRoot), nvimBin)
}

// testServerWith is testServer for the cases that need their own providers.json.
func testServerWith(t *testing.T, cfg, nvimBin string) *Server {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	inst := filepath.Join(state, "instances")
	sock := filepath.Join(state, "sock")
	for _, d := range []string{inst, sock} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(base, "providers.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Options{
		ConfigPath:  cfgPath,
		StateDir:    state,
		InstanceDir: inst,
		SockDir:     sock,
		TokenPath:   filepath.Join(state, "token"),
		NvimBin:     nvimBin,
		Port:        testPort,
		Version:     "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// addInstance writes a registry entry for a live pid with a real socket file.
func addInstance(t *testing.T, srv *Server, name, cwd string) {
	t.Helper()
	sockPath := filepath.Join(srv.opts.SockDir, name+".sock")
	if err := os.WriteFile(sockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"v": 1, "pid": os.Getpid(), "servername": sockPath,
		"cwd": cwd, "launch_cwd": cwd, "root": cwd, "label": name,
		"spawn_id": nil, "started_at": 1, "last_focused": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.opts.InstanceDir, name+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// noFocus keeps every test off AppleScript: maybeFocus short-circuits when
// focus is explicitly false.
func noFocus() *bool { b := false; return &b }

// TestOpenExistingSkipsDisallowedCandidate is the regression for the broken
// fallback chain: a candidate whose resolved path lies outside the allowlist
// must be SKIPPED, not abort the whole loop, so a good later candidate is
// still reached.
func TestOpenExistingSkipsDisallowedCandidate(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(filepath.Join(d, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "lib", "x.go"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin, _ := fakeNvim(t, base, `{"ok":true}`)
	srv := testServer(t, allowed, bin)

	// "bad" is the explicit target and resolves fine, but sits outside the
	// allowlist. "good" is the fallback that must be reached.
	addInstance(t, srv, "bad", outside)
	addInstance(t, srv, "good", allowed)

	list, _ := srv.reg.List()
	var badID string
	for _, i := range list {
		if i.Label == "bad" {
			badID = i.ID()
		}
	}
	if badID == "" {
		t.Fatal("bad instance was not registered")
	}

	resp := srv.open(context.Background(), openRequest{
		Mode: "existing", Target: badID, RepoPath: "lib/x.go", Focus: noFocus(),
	})
	if !resp.OK {
		t.Fatalf("expected the loop to fall through to the allowed instance, got %+v", resp)
	}
	if resp.Focused != "none" {
		t.Errorf("focused = %q, want none (test must not touch AppleScript)", resp.Focused)
	}
}

// TestOpenExistingFailsWhenEveryCandidateDisallowed checks the loop still
// reports failure once genuinely exhausted, rather than silently succeeding.
func TestOpenExistingFailsWhenEveryCandidateDisallowed(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "lib", "x.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeNvim(t, base, `{"ok":true}`)
	srv := testServer(t, allowed, bin)
	addInstance(t, srv, "bad", outside)

	list, _ := srv.reg.List()
	resp := srv.open(context.Background(), openRequest{
		Mode: "existing", Target: list[0].ID(), RepoPath: "lib/x.go", Focus: noFocus(),
	})
	if resp.OK {
		t.Fatalf("expected failure when every candidate is disallowed, got %+v", resp)
	}
	if resp.Code != CodeInstanceGone && resp.Code != CodeFileNotFound {
		t.Errorf("code = %q, want an exhaustion code", resp.Code)
	}
}

func TestOpenExistingHappyPathSendsSnakeCasePayload(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(filepath.Join(allowed, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "lib", "x.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, logPath := fakeNvim(t, base, `{"ok":true}`)
	srv := testServer(t, allowed, bin)
	addInstance(t, srv, "good", allowed)

	list, _ := srv.reg.List()
	line, end := 12, 20
	resp := srv.open(context.Background(), openRequest{
		Mode: "existing", Target: list[0].ID(), RepoPath: "lib/x.go",
		Line: &line, EndLine: &end, Focus: noFocus(),
	})
	if !resp.OK {
		t.Fatalf("expected success, got %+v", resp)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// The Lua side's contract is snake_case, unlike the HTTP API. The payload
	// also has to arrive inside a vimscript single-quoted string.
	for _, want := range []string{
		"--remote-expr",
		`luaeval('_G.__codelink_rpc(_A)'`,
		`"line":12`,
		`"end_line":20`,
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("nvim argv missing %q\ngot: %s", want, args)
		}
	}
}

// TestOpenExistingReportsNvimError checks an application-level failure (exit 0,
// ok:false in the body) surfaces as NVIM_ERROR rather than success.
func TestOpenExistingReportsNvimError(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(filepath.Join(allowed, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "lib", "x.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeNvim(t, base, `{"ok":false,"error":"buffer is modified"}`)
	srv := testServer(t, allowed, bin)
	addInstance(t, srv, "good", allowed)

	list, _ := srv.reg.List()
	resp := srv.open(context.Background(), openRequest{
		Mode: "existing", Target: list[0].ID(), RepoPath: "lib/x.go", Focus: noFocus(),
	})
	if resp.OK || resp.Code != CodeNvimError {
		t.Fatalf("want NVIM_ERROR, got %+v", resp)
	}
	if resp.Error != "buffer is modified" {
		t.Errorf("error = %q, want the nvim-supplied message", resp.Error)
	}
}

// TestOpenNewRejectsUnconfiguredRoot proves the caller cannot choose where the
// editor is launched: with opt.exrc = true that choice decides which trusted
// project config nvim reaches, from the cwd upwards.
func TestOpenNewRejectsUnconfiguredRoot(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeNvim(t, base, `{"ok":true}`)
	srv := testServer(t, allowed, bin)

	for _, target := range []string{"/etc", "/", filepath.Join(allowed, "..")} {
		resp := srv.open(context.Background(), openRequest{
			Mode: "new", Target: target, RepoPath: "passwd", Focus: noFocus(),
		})
		if resp.OK || resp.Code != CodeRootNotAllowed {
			t.Errorf("target %q: want ROOT_NOT_ALLOWED, got %+v", target, resp)
		}
	}
}

// TestSpawnWorkdirRejectsSymlinkEscape guards that same spawn cwd one step
// further in: the target root is legitimate, but the project directory
// derived from repoPath is a symlink out of it, and filepath.Join's lexical
// Clean does not notice. The check on the resolved FILE cannot catch this — the
// fixture deliberately arranges for that check to pass — so the cwd needs one of
// its own.
func TestSpawnWorkdirRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{
		filepath.Join(allowed, "real"),
		filepath.Join(allowed, "proj", "lib"),
		filepath.Join(outside, "lib"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(allowed, "real", "x.go"),
		filepath.Join(allowed, "proj", "lib", "x.go"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The escape: a directory symlink leading out of the root, and behind it a
	// file symlinked straight back into the root. Everything the daemon stats
	// then canonicalises to somewhere allowed — everything except the directory
	// nvim would actually be launched in.
	if err := os.Symlink(outside, filepath.Join(allowed, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(allowed, "real", "x.go"), filepath.Join(outside, "lib", "x.go")); err != nil {
		t.Fatal(err)
	}

	bin, _ := fakeNvim(t, base, `{"ok":true}`)
	srv := testServer(t, allowed, bin)
	allRoots := srv.roots.ExpandAll(srv.Config())
	root, ok := roots.Allowed(allRoots, allowed)
	if !ok {
		t.Fatal("precondition: the temp directory must be a configured root")
	}
	// Precondition for the escape case, and the reason it is not already
	// covered: the file check openNew performs today is satisfied.
	local, _, ok := resolve.Resolve("evil/lib/x.go", allowed)
	if !ok || !roots.PathAllowed(allRoots, local) {
		t.Fatalf("precondition: evil/lib/x.go must resolve to an ALLOWED path, got %q ok=%v", local, ok)
	}

	tests := []struct {
		name     string
		repoPath string
		want     string
	}{
		{
			name:     "project directory inside the root is used",
			repoPath: "proj/lib/x.go", want: filepath.Join(allowed, "proj"),
		},
		{
			name:     "project directory symlinked out of the root falls back to the root",
			repoPath: "evil/lib/x.go", want: allowed,
		},
		{
			name:     "no project directory in the repo path",
			repoPath: "x.go", want: allowed,
		},
		{
			name:     "project directory does not exist locally",
			repoPath: "ghost/lib/x.go", want: allowed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.spawnWorkdir(root, tc.repoPath, allRoots); got != tc.want {
				t.Errorf("spawnWorkdir(%q) = %q, want %q", tc.repoPath, got, tc.want)
			}
		})
	}
}

// repoServer builds a server over two same-shaped checkouts plus one that the
// roots glob cannot reach, and a provider that captures a repo group on /code/
// but not on /src/ — so one config exercises both the filtered and the
// historical unfiltered path. /o/ additionally captures an owner, and repoPage
// recognises the repo-level pages /repostatus answers about; both are inert for
// the /code/ and /src/ cases.
//
// aliasRel, when set, is registered as the repoAliases target for "widgets".
func repoServer(t *testing.T, aliasRel string) (srv *Server, base string) {
	t.Helper()
	base = t.TempDir()
	for _, d := range []string{"checkouts/synapses", "checkouts/codelink", "elsewhere/neurons"} {
		dir := filepath.Join(base, filepath.FromSlash(d), "lib")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	aliases := ""
	if aliasRel != "" {
		aliases = fmt.Sprintf(`,"repoAliases":{"widgets":%q}`,
			filepath.Join(base, filepath.FromSlash(aliasRel)))
	}
	bin, _ := fakeNvim(t, base, `{"ok":true}`)
	srv = testServerWith(t, fmt.Sprintf(`{
      "version":1,"extensionId":"testext",
      "providers":[{"id":"t","hosts":["*.example.com"],
        "match":[
          {"path":"^/code/(?P<repo>[^/]+)/(?P<repoPath>.+)$"},
          {"path":"^/src/(?P<repoPath>.+)$"},
          {"path":"^/o/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(?P<repoPath>.+)$"}],
        "repoPage":"^/repo/(?P<owner>[^/]+)/(?P<repo>[^/]+)(?:/tree/.*)?/?$",
        "projectMarkers":["lib"],
        "roots":[{"glob":%q}]%s}]}`,
		filepath.Join(base, "checkouts", "*"), aliases), bin)
	return srv, base
}

func candidateRoots(resp resolveResponse) []string {
	out := make([]string, 0, len(resp.RootCandidates))
	for _, c := range resp.RootCandidates {
		out = append(out, filepath.Base(c.Root))
	}
	slices.Sort(out)
	return out
}

func instanceRoots(resp resolveResponse) []string {
	out := make([]string, 0, len(resp.OpenInstances))
	for _, i := range resp.OpenInstances {
		out = append(out, filepath.Base(i.Root))
	}
	slices.Sort(out)
	return out
}

// TestResolveFiltersByRepo is the regression for a link to repo A opening the
// same-named file in checkout B — through the root candidates AND through an
// nvim already running in the wrong checkout, which outranks every candidate.
func TestResolveFiltersByRepo(t *testing.T) {
	srv, base := repoServer(t, "")
	addInstance(t, srv, "synapses", filepath.Join(base, "checkouts", "synapses"))
	addInstance(t, srv, "codelink", filepath.Join(base, "checkouts", "codelink"))

	tests := []struct {
		name          string
		url           string
		wantCands     []string
		wantInstances []string
		wantWarning   string
	}{
		{
			name: "repo picks its own checkout", url: "https://a.example.com/code/synapses/lib/x.go",
			wantCands: []string{"synapses"}, wantInstances: []string{"synapses"},
		},
		{
			name: "and the other repo picks the other one", url: "https://a.example.com/code/codelink/lib/x.go",
			wantCands: []string{"codelink"}, wantInstances: []string{"codelink"},
		},
		{
			name: "the repo name is matched case-insensitively", url: "https://a.example.com/code/Synapses/lib/x.go",
			wantCands: []string{"synapses"}, wantInstances: []string{"synapses"},
		},
		{
			// The match entry captures no repo group, so nothing is filtered —
			// this is the behaviour every existing provider keeps.
			name:      "a URL with no repo group still reaches every checkout",
			url:       "https://a.example.com/src/lib/x.go",
			wantCands: []string{"codelink", "synapses"}, wantInstances: []string{"codelink", "synapses"},
		},
		{
			name:        "a repo with no checkout is reported, not fallen back from",
			url:         "https://a.example.com/code/ghost/lib/x.go",
			wantWarning: `repo "ghost" matched no local checkout`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.resolveURL(tc.url)
			if !resp.OK {
				t.Fatalf("resolve(%q) not ok: %+v", tc.url, resp)
			}
			if got := candidateRoots(resp); !slices.Equal(got, tc.wantCands) {
				t.Errorf("rootCandidates = %v, want %v", got, tc.wantCands)
			}
			if got := instanceRoots(resp); !slices.Equal(got, tc.wantInstances) {
				t.Errorf("openInstances = %v, want %v", got, tc.wantInstances)
			}
			warned := slices.Contains(resp.Warnings, tc.wantWarning)
			if tc.wantWarning != "" && !warned {
				t.Errorf("warnings = %v, want one of them to be %q", resp.Warnings, tc.wantWarning)
			}
		})
	}
}

// TestResolveRepoWarningDistinguishesMissingCheckoutFromMissingFile pins the two
// apart: both end in an empty answer, but "the repo has no checkout here" and
// "this checkout does not have that file (yet)" send the reader after entirely
// different things.
func TestResolveRepoWarningDistinguishesMissingCheckoutFromMissingFile(t *testing.T) {
	srv, _ := repoServer(t, "")
	const genericWarning = "no local checkout contains this path"

	tests := []struct {
		name     string
		url      string
		wantRepo string // the repo warning expected verbatim, "" for none at all
	}{
		{
			name: "the checkout exists, the file does not",
			url:  "https://a.example.com/code/synapses/lib/missing.go",
		},
		{
			name:     "no checkout survives the filter",
			url:      "https://a.example.com/code/ghost/lib/x.go",
			wantRepo: `repo "ghost" matched no local checkout`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.resolveURL(tc.url)
			if !resp.OK {
				t.Fatalf("resolve(%q) not ok: %+v", tc.url, resp)
			}
			// Both cases end in nothing to open, so the generic warning is owed
			// either way; only the repo one is in question.
			if !slices.Contains(resp.Warnings, genericWarning) {
				t.Errorf("warnings = %v, want the generic %q", resp.Warnings, genericWarning)
			}
			anyRepo := slices.ContainsFunc(resp.Warnings, func(w string) bool {
				return strings.HasPrefix(w, `repo "`)
			})
			switch {
			case tc.wantRepo == "" && anyRepo:
				t.Errorf("warnings = %v, want no repo warning: the checkout was there, the file was not", resp.Warnings)
			case tc.wantRepo != "" && !slices.Contains(resp.Warnings, tc.wantRepo):
				t.Errorf("warnings = %v, want one of them to be %q", resp.Warnings, tc.wantRepo)
			}
		})
	}
}

// TestResolveRepoAliasReachesUnenumeratedCheckout covers the alias half: the
// target is named after nothing in particular and no roots entry enumerates it,
// yet it is the only checkout that may serve the repo.
func TestResolveRepoAliasReachesUnenumeratedCheckout(t *testing.T) {
	srv, base := repoServer(t, "elsewhere/neurons")
	alias := filepath.Join(base, "elsewhere", "neurons")
	addInstance(t, srv, "neurons", alias)
	addInstance(t, srv, "synapses", filepath.Join(base, "checkouts", "synapses"))

	resp := srv.resolveURL("https://a.example.com/code/widgets/lib/x.go")
	if !resp.OK {
		t.Fatalf("resolve not ok: %+v", resp)
	}
	if got := candidateRoots(resp); !slices.Equal(got, []string{"neurons"}) {
		t.Errorf("rootCandidates = %v, want [neurons]", got)
	}
	if got := instanceRoots(resp); !slices.Equal(got, []string{"neurons"}) {
		t.Errorf("openInstances = %v, want [neurons]", got)
	}
}

// TestOpenNewAcceptsAliasTarget checks the /open allowlist grew with the
// aliases: a checkout the daemon offers as a candidate but refuses to spawn in
// would be a dead button. repoPath is deliberately missing so the request stops
// at the file check, before any terminal is spawned.
func TestOpenNewAcceptsAliasTarget(t *testing.T) {
	srv, base := repoServer(t, "elsewhere/neurons")

	resp := srv.open(context.Background(), openRequest{
		Mode: "new", Target: filepath.Join(base, "elsewhere", "neurons"),
		RepoPath: "no/such/file.go", Focus: noFocus(),
	})
	if resp.OK || resp.Code != CodeFileNotFound {
		t.Errorf("alias target: want FILE_NOT_FOUND (i.e. past the allowlist), got %+v", resp)
	}
	// Its parent is neither a root nor an alias target, so it stays rejected.
	resp = srv.open(context.Background(), openRequest{
		Mode: "new", Target: filepath.Join(base, "elsewhere"),
		RepoPath: "neurons/lib/x.go", Focus: noFocus(),
	})
	if resp.OK || resp.Code != CodeRootNotAllowed {
		t.Errorf("alias parent: want ROOT_NOT_ALLOWED, got %+v", resp)
	}
}

// TestResolveOutcomeCodes covers the machine-readable split of the one outcome
// that used to be a single shrug: empty lists. "Clone this repository" and
// "this checkout does not have that file" are different instructions, and the
// extension cannot be asked to recover the difference by parsing prose.
func TestResolveOutcomeCodes(t *testing.T) {
	srv, base := repoServer(t, "elsewhere/neurons")
	addInstance(t, srv, "synapses", filepath.Join(base, "checkouts", "synapses"))

	tests := []struct {
		name string
		url  string
		want string // "" means the response must carry no code at all
	}{
		{
			name: "no checkout is named after the repo",
			url:  "https://a.example.com/code/ghost/lib/x.go", want: CodeRepoNotLocal,
		},
		{
			name: "the checkout is here, the file is not",
			url:  "https://a.example.com/code/synapses/lib/missing.go", want: CodeFileNotLocal,
		},
		{
			// The alias supplies an eligible root, so the repo IS local; only the
			// file is missing. Deciding on the empty result rather than on the
			// surviving roots would get this one backwards.
			name: "an aliased checkout is local even when the file is not",
			url:  "https://a.example.com/code/widgets/lib/missing.go", want: CodeFileNotLocal,
		},
		{
			// Nothing was filtered, so there is no repo-level claim to make.
			name: "a provider capturing no repo gets no code",
			url:  "https://a.example.com/src/lib/missing.go", want: "",
		},
		{
			name: "a resolve with something to open gets no code",
			url:  "https://a.example.com/code/synapses/lib/x.go", want: "",
		},
		{
			name: "an owner-capturing URL is judged on its repo like any other",
			url:  "https://a.example.com/o/acme/ghost/lib/x.go", want: CodeRepoNotLocal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.resolveURL(tc.url)
			if !resp.OK {
				t.Fatalf("resolve(%q) not ok: %+v", tc.url, resp)
			}
			if resp.Code != tc.want {
				t.Errorf("code = %q, want %q (warnings: %v)", resp.Code, tc.want, resp.Warnings)
			}
			// The warnings are the human half of the same answer and predate the
			// code; neither may quietly replace the other.
			if len(resp.OpenInstances) == 0 && len(resp.RootCandidates) == 0 &&
				!slices.Contains(resp.Warnings, "no local checkout contains this path") {
				t.Errorf("warnings = %v, want the generic one to survive alongside the code", resp.Warnings)
			}
		})
	}
}

// TestResolvePayloadUnchangedWithoutRepo is the compatibility guard for both new
// Parsed fields: a provider that captures neither repo nor owner must serialise
// byte-for-byte as it did before they existed.
func TestResolvePayloadUnchangedWithoutRepo(t *testing.T) {
	srv, _ := repoServer(t, "")
	for _, u := range []string{
		"https://a.example.com/src/lib/x.go",
		"https://a.example.com/src/lib/missing.go",
	} {
		raw, err := json.Marshal(srv.resolveURL(u))
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"code"`, `"repo"`, `"owner"`} {
			if strings.Contains(string(raw), key) {
				t.Errorf("resolve(%q) payload carries %s with nothing to say: %s", u, key, raw)
			}
		}
	}
}

// TestResolveEchoesOwner: the owner is carried through untouched and changes
// nothing about which checkout answers — no checkout records the namespace it
// was cloned from, so resolving on it would be a guess.
func TestResolveEchoesOwner(t *testing.T) {
	srv, base := repoServer(t, "")
	addInstance(t, srv, "synapses", filepath.Join(base, "checkouts", "synapses"))

	resp := srv.resolveURL("https://a.example.com/o/acme/synapses/lib/x.go")
	if !resp.OK {
		t.Fatalf("resolve not ok: %+v", resp)
	}
	if resp.Parsed.Owner != "acme" {
		t.Errorf("owner = %q, want acme", resp.Parsed.Owner)
	}
	if resp.Parsed.Repo != "synapses" {
		t.Errorf("repo = %q, want synapses", resp.Parsed.Repo)
	}
	if got := candidateRoots(resp); !slices.Equal(got, []string{"synapses"}) {
		t.Errorf("rootCandidates = %v, want [synapses]", got)
	}

	// A different owner over the same repo name must resolve identically: the
	// owner is echoed, never filtered on.
	other := srv.resolveURL("https://a.example.com/o/other-org/synapses/lib/x.go")
	if got := candidateRoots(other); !slices.Equal(got, candidateRoots(resp)) {
		t.Errorf("owner changed the answer: %v vs %v", got, candidateRoots(resp))
	}
	if other.Parsed.Owner != "other-org" {
		t.Errorf("owner = %q, want other-org", other.Parsed.Owner)
	}
}

// authGet performs an authenticated GET through the full handler chain and
// decodes the body as a bare map, so a test can assert on the keys that are
// present as well as their values.
func authGet(t *testing.T, srv *Server, target string) (int, map[string]any) {
	t.Helper()
	t.Setenv("CODELINK_QUIET", "1")
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	req.Header.Set("X-Codelink-Client", "ext")
	req.Header.Set("X-Codelink-Token", srv.token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: body is not JSON: %v (%s)", target, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestRepoStatus pins all four answer shapes. The keys are asserted exactly,
// not just the values: the miss shapes must NOT carry local/roots, or a caller
// would read "this page is not a repo page" as "this repo is not checked out".
func TestRepoStatus(t *testing.T) {
	srv, base := repoServer(t, "elsewhere/neurons")

	tests := []struct {
		name      string
		url       string
		wantKeys  []string
		wantCode  string
		wantLocal bool
		wantOwner string
		wantRepo  string
		wantRoots []string
	}{
		{
			name: "a checked-out repository", url: "https://a.example.com/repo/acme/synapses",
			wantKeys:  []string{"local", "ok", "owner", "provider", "repo", "roots"},
			wantLocal: true, wantOwner: "acme", wantRepo: "synapses",
			wantRoots: []string{filepath.Join(base, "checkouts", "synapses")},
		},
		{
			name: "a tree page deeper in the same repository", url: "https://a.example.com/repo/acme/synapses/tree/main/lib",
			wantKeys:  []string{"local", "ok", "owner", "provider", "repo", "roots"},
			wantLocal: true, wantOwner: "acme", wantRepo: "synapses",
			wantRoots: []string{filepath.Join(base, "checkouts", "synapses")},
		},
		{
			// No file was probed anywhere here — the repo page names none — so
			// this is purely "which checkouts could serve this repository".
			name: "a repository nothing local serves", url: "https://a.example.com/repo/acme/ghost",
			wantKeys: []string{"code", "local", "ok", "owner", "provider", "repo", "roots"},
			wantCode: CodeRepoNotLocal, wantLocal: false, wantOwner: "acme", wantRepo: "ghost",
			wantRoots: []string{},
		},
		{
			name: "a repository reached only through an alias", url: "https://a.example.com/repo/acme/widgets",
			wantKeys:  []string{"local", "ok", "owner", "provider", "repo", "roots"},
			wantLocal: true, wantOwner: "acme", wantRepo: "widgets",
			wantRoots: []string{filepath.Join(base, "elsewhere", "neurons")},
		},
		{
			name: "a file link is not a repo page", url: "https://a.example.com/code/synapses/lib/x.go",
			wantKeys: []string{"code", "ok"}, wantCode: CodeNotARepoPage,
		},
		{
			name: "a page on the host that matches no repoPage form", url: "https://a.example.com/settings",
			wantKeys: []string{"code", "ok"}, wantCode: CodeNotARepoPage,
		},
		{
			name: "a host no provider claims", url: "https://code.other-host.org/repo/acme/synapses",
			wantKeys: []string{"code", "ok"}, wantCode: CodeNoProvider,
		},
		{
			// url.Parse hands this a host, but the daemon never treats a scheme
			// the browser would not navigate as a code host — same rule /resolve
			// applies.
			name: "a javascript: URL is not a code host", url: "javascript://a.example.com/repo/acme/synapses",
			wantKeys: []string{"code", "ok"}, wantCode: CodeNoProvider,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := authGet(t, srv, "/repostatus?url="+url.QueryEscape(tc.url))
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if got := keysOf(body); !slices.Equal(got, tc.wantKeys) {
				t.Fatalf("keys = %v, want %v (body %v)", got, tc.wantKeys, body)
			}
			wantOK := tc.wantCode != CodeNotARepoPage && tc.wantCode != CodeNoProvider
			if body["ok"] != wantOK {
				t.Errorf("ok = %v, want %v", body["ok"], wantOK)
			}
			if tc.wantCode != "" && body["code"] != tc.wantCode {
				t.Errorf("code = %v, want %q", body["code"], tc.wantCode)
			}
			if !wantOK {
				return
			}
			if body["provider"] != "t" {
				t.Errorf("provider = %v, want t", body["provider"])
			}
			if body["owner"] != tc.wantOwner {
				t.Errorf("owner = %v, want %q", body["owner"], tc.wantOwner)
			}
			if body["repo"] != tc.wantRepo {
				t.Errorf("repo = %v, want %q", body["repo"], tc.wantRepo)
			}
			if body["local"] != tc.wantLocal {
				t.Errorf("local = %v, want %v", body["local"], tc.wantLocal)
			}
			var gotRoots []string
			for _, r := range body["roots"].([]any) {
				entry := r.(map[string]any)
				if !slices.Equal(keysOf(entry), []string{"label", "root"}) {
					t.Errorf("roots entry keys = %v, want [label root]", keysOf(entry))
				}
				gotRoots = append(gotRoots, entry["root"].(string))
			}
			slices.Sort(gotRoots)
			if len(gotRoots) != len(tc.wantRoots) {
				t.Fatalf("roots = %v, want %v", gotRoots, tc.wantRoots)
			}
			for i := range gotRoots {
				if gotRoots[i] != tc.wantRoots[i] {
					t.Errorf("roots[%d] = %q, want %q", i, gotRoots[i], tc.wantRoots[i])
				}
			}
		})
	}
}

// TestRepoStatusRejectsMissingURL: the endpoint answers about a page, so with no
// page named there is nothing to answer — and that is a caller bug, not a "no
// provider" verdict about some URL.
func TestRepoStatusRejectsMissingURL(t *testing.T) {
	srv, _ := repoServer(t, "")
	status, body := authGet(t, srv, "/repostatus")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["ok"] != false || body["code"] != CodeBadRequest {
		t.Errorf("body = %v, want ok=false code=%s", body, CodeBadRequest)
	}
}

// TestRepoStatusRequiresAuth: the new endpoint reads the same providers config
// and root layout as every other one, so it has to sit behind the same guard. It
// is routed through the shared middleware precisely so this cannot be forgotten,
// and this test is what proves the routing did not bypass it.
func TestRepoStatusRequiresAuth(t *testing.T) {
	srv, _ := repoServer(t, "")
	t.Setenv("CODELINK_QUIET", "1")
	h := srv.Handler()

	tests := []struct {
		name   string
		client string
		token  string
		host   string
		want   int
	}{
		{"fully authenticated", "ext", srv.token, fmt.Sprintf("127.0.0.1:%d", testPort), http.StatusOK},
		{"no credentials at all", "", "", fmt.Sprintf("127.0.0.1:%d", testPort), http.StatusForbidden},
		{"wrong token", "ext", "deadbeef", fmt.Sprintf("127.0.0.1:%d", testPort), http.StatusForbidden},
		{"missing client header", "", srv.token, fmt.Sprintf("127.0.0.1:%d", testPort), http.StatusForbidden},
		{"rebound attacker host", "ext", srv.token, fmt.Sprintf("evil.example:%d", testPort), http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/repostatus?url=https://a.example.com/repo/acme/synapses", nil)
			req.Host = tc.host
			if tc.client != "" {
				req.Header.Set("X-Codelink-Client", tc.client)
			}
			if tc.token != "" {
				req.Header.Set("X-Codelink-Token", tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestRepoStatusCORSPinsTheExtensionOrigin: the CORS policy IS the anti-CSRF
// mechanism, so a page on a provider host — which is exactly where the extension
// runs its content script — must get no Access-Control headers from the new
// endpoint either.
func TestRepoStatusCORSPinsTheExtensionOrigin(t *testing.T) {
	srv, _ := repoServer(t, "")
	t.Setenv("CODELINK_QUIET", "1")
	h := srv.Handler()

	tests := []struct {
		name       string
		origin     string
		wantAllow  string
		wantStatus int
	}{
		{"the extension itself", "chrome-extension://testext", "chrome-extension://testext", http.StatusOK},
		{"a page on a provider host", "https://a.example.com", "", http.StatusOK},
		{"another extension", "chrome-extension://someoneelse", "", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/repostatus?url=https://a.example.com/repo/acme/synapses", nil)
			req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-Codelink-Client", "ext")
			req.Header.Set("X-Codelink-Token", srv.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
		})
	}
}

// TestHealthIsTheLivenessEndpoint pins the cheap "is the daemon up, and which
// build" contract a popup or toolbar icon polls. There is deliberately no
// separate /status: /health already answers exactly that, with ok/version/pid,
// and a second endpoint saying the same thing is one more thing to keep in sync.
func TestHealthIsTheLivenessEndpoint(t *testing.T) {
	srv, _ := repoServer(t, "")
	status, body := authGet(t, srv, "/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["version"] != "test" {
		t.Errorf("version = %v, want the build the server was configured with", body["version"])
	}
	if pid, ok := body["pid"].(float64); !ok || int(pid) != os.Getpid() {
		t.Errorf("pid = %v, want %d", body["pid"], os.Getpid())
	}
}

// TestGuardRejectsForeignHost covers the DNS-rebinding shape. Such a request is
// same-origin with the daemon, so it carries no Origin header and needs no
// preflight — the CORS policy never sees it. The Host header is the part the
// attacker cannot rewrite.
func TestGuardRejectsForeignHost(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeNvim(t, base, `{"ok":true}`)
	srv := testServer(t, allowed, bin)
	// The access log is built into the handler chain; silence it here so a
	// table of expected 403s does not read like a test failure.
	t.Setenv("CODELINK_QUIET", "1")
	h := srv.Handler()

	tests := []struct {
		name string
		host string
		want int
	}{
		{"the address the extension actually fetches", fmt.Sprintf("127.0.0.1:%d", testPort), http.StatusOK},
		{"the loopback name", fmt.Sprintf("localhost:%d", testPort), http.StatusOK},
		{"a rebound attacker name on our port", fmt.Sprintf("evil.example:%d", testPort), http.StatusForbidden},
		{"loopback on a port we do not listen on", "127.0.0.1:1234", http.StatusForbidden},
		{"no port at all", "127.0.0.1", http.StatusForbidden},
		{"ipv6 loopback, which Serve deliberately never binds", fmt.Sprintf("[::1]:%d", testPort), http.StatusForbidden},
		{"empty host", "", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Everything else about the request is valid, so only the Host can
			// account for a rejection.
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			req.Host = tc.host
			req.Header.Set("X-Codelink-Client", "ext")
			req.Header.Set("X-Codelink-Token", srv.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("Host %q -> %d, want %d", tc.host, rec.Code, tc.want)
			}
		})
	}
}

// preexisting creates a file with a mode wider than 0600. os.WriteFile's mode
// is subject to the umask, so it is forced afterwards.
func preexisting(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("stale"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestWriteTokenJSAlwaysNarrowsMode is the regression for os.WriteFile applying
// its mode only on CREATE: a token.gen.js already in the checkout kept whatever
// permissions it had, leaving the shared secret readable by every account on the
// machine.
func TestWriteTokenJSAlwaysNarrowsMode(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{name: "file does not exist yet"},
		{
			name:    "pre-existing world-readable file",
			prepare: func(t *testing.T, path string) { preexisting(t, path, 0o644) },
		},
		{
			name:    "pre-existing world-writable file",
			prepare: func(t *testing.T, path string) { preexisting(t, path, 0o666) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token.gen.js")
			if tc.prepare != nil {
				tc.prepare(t, path)
			}
			if err := writeTokenJS(path, "deadbeef"); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("mode = %04o, want 0600", got)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if want := "self.CODELINK_TOKEN = 'deadbeef';\n"; string(raw) != want {
				t.Errorf("body = %q, want %q", raw, want)
			}
		})
	}
}
