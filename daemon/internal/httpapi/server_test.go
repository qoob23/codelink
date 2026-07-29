package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	cfg := fmt.Sprintf(`{
      "version":1,"extensionId":"testext",
      "providers":[{"id":"t","hosts":["*.example.com"],
        "match":[{"path":"^/src/(?P<repoPath>.+)$"}],
        "hash":"^L(?P<line>\\d+)$",
        "projectMarkers":["lib"],
        "roots":[{"path":%q}]}]}`, allowedRoot)
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
