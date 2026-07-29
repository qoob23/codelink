package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		Port:        0,
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

// TestOpenNewRejectsUnconfiguredRoot guards the opt.exrc RCE surface.
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
