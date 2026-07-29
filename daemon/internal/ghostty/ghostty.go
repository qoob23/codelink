// Package ghostty spawns and focuses terminal windows via AppleScript.
//
// Terminology follows /Applications/Ghostty.app/Contents/Resources/Ghostty.sdef
// (Ghostty 1.3.1): the application has `terminals`, a window has a
// `selected tab`, a tab has a `focused terminal`, and a terminal exposes `id`
// and `working directory` and responds to `focus`.
//
// Every osascript call is best-effort. A TCC (Automation permission) denial
// must degrade the experience, never break it: spawning falls back to
// `open -na Ghostty.app`, and focusing falls back to simply activating the app.
package ghostty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"codelink/internal/resolve"
)

// Focus tiers, most precise first.
const (
	FocusExact     = "exact"     // we know the terminal id we spawned
	FocusHeuristic = "heuristic" // matched a unique terminal by working directory
	FocusApp       = "app"       // could only raise the application
	FocusNone      = "none"      // focusing was not attempted
)

const osascriptBin = "/usr/bin/osascript"
const appPath = "/Applications/Ghostty.app"

// Client owns the spawn_id -> terminal id map used for exact focusing.
type Client struct {
	mu       sync.RWMutex
	terminal map[string]string // spawn id -> Ghostty terminal id

	// Timeout bounds a single osascript invocation.
	Timeout time.Duration
}

// New returns a client with sane defaults.
func New() *Client {
	return &Client{terminal: map[string]string{}, Timeout: 10 * time.Second}
}

// NewSpawnID returns a fresh random spawn id.
func NewSpawnID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// quote escapes a Go string for embedding in an AppleScript double-quoted
// literal.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func (c *Client) runScript(ctx context.Context, script string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, osascriptBin, "-e", script)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("osascript: %s", firstLine(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Spawn opens a new Ghostty window running nvim in workdir, tagged with
// spawnID so the daemon can find the resulting registry entry.
//
// It returns the new terminal's id when AppleScript succeeded (enabling tier-1
// focusing later) and an empty id when it had to fall back to `open`.
func (c *Client) Spawn(ctx context.Context, workdir, nvimBin, spawnID string) (string, error) {
	script := fmt.Sprintf(`tell application "Ghostty"
  set cfg to new surface configuration
  set initial working directory of cfg to %s
  set command of cfg to %s
  set environment variables of cfg to {%s}
  set w to new window with configuration cfg
  get id of focused terminal of selected tab of w
end tell`,
		quote(workdir),
		quote(nvimBin),
		quote("CODELINK_SPAWN_ID="+spawnID),
	)

	id, err := c.runScript(ctx, script)
	if err == nil && id != "" {
		c.mu.Lock()
		c.terminal[spawnID] = id
		c.mu.Unlock()
		return id, nil
	}

	// Fallback for a denied or broken Automation permission. It cannot carry
	// the spawn id, so the caller will not find a tagged registry entry and
	// will simply report a best-effort open.
	if ferr := c.spawnFallback(ctx, workdir, nvimBin); ferr != nil {
		if err == nil {
			return "", ferr
		}
		return "", fmt.Errorf("%v (fallback: %v)", err, ferr)
	}
	return "", nil
}

func (c *Client) spawnFallback(ctx context.Context, workdir, nvimBin string) error {
	// ghostty(1) documents --working-directory=<directory>, and every CLI
	// example uses --flag=value. Passing the value as a separate argv element
	// leaves the flag valueless and the directory a stray positional.
	// -e stays last: it consumes the remainder of the command line.
	cmd := exec.CommandContext(ctx, "open", "-na", appPath, "--args",
		"--working-directory="+workdir, "-e", nvimBin)
	return cmd.Run()
}

// FocusTerminalID focuses a specific terminal surface (tier 1).
func (c *Client) FocusTerminalID(ctx context.Context, id string) error {
	script := fmt.Sprintf(`tell application "Ghostty"
  activate
  focus (first terminal whose id is %s)
end tell`, quote(id))
	_, err := c.runScript(ctx, script)
	return err
}

// TerminalIDFor returns the terminal id recorded for a spawn id, if any.
func (c *Client) TerminalIDFor(spawnID string) (string, bool) {
	if spawnID == "" {
		return "", false
	}
	c.mu.RLock()
	id, ok := c.terminal[spawnID]
	c.mu.RUnlock()
	return id, ok
}

// terminalRecord is one row of the listTerminals dump.
type terminalRecord struct {
	ID  string
	Cwd string
}

// listTerminals dumps every terminal's id and working directory.
func (c *Client) listTerminals(ctx context.Context) ([]terminalRecord, error) {
	// Two parallel lists are emitted and zipped, because AppleScript's
	// `text item delimiters` flattening is the only reliable way to get
	// structured output back out of osascript.
	script := `tell application "Ghostty"
  set ids to {}
  set dirs to {}
  repeat with t in terminals
    set end of ids to (id of t as text)
    try
      set end of dirs to (working directory of t as text)
    on error
      set end of dirs to ""
    end try
  end repeat
end tell
set AppleScript's text item delimiters to linefeed
return (ids as text) & linefeed & "--" & linefeed & (dirs as text)`

	out, err := c.runScript(ctx, script)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(out, "\n--\n", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	ids := splitLines(parts[0])
	dirs := splitLines(parts[1])
	n := min(len(ids), len(dirs))
	recs := make([]terminalRecord, 0, n)
	for i := range n {
		recs = append(recs, terminalRecord{ID: ids[i], Cwd: dirs[i]})
	}
	return recs, nil
}

// FocusByWorkingDir focuses the unique terminal whose working directory matches
// launchCwd (tier 2).
//
// Ghostty's `working directory` comes from OSC 7, i.e. the shell's LOGICAL
// $PWD, and it freezes at the directory the shell was in when nvim launched —
// which is exactly the registry's launch_cwd. But launch_cwd is written from
// vim.fn.getcwd(), which is PHYSICAL (symlink-resolved). Comparing the two
// verbatim silently misses on any symlinked directory, and ~/.config is a stow
// symlink on this machine, so both sides are canonicalised first.
//
// Focus is only issued when exactly one terminal matches; anything ambiguous
// falls through to the caller's tier-3 handling.
//
// KNOWN LIMITATION: two nvim tabs open in the same worktree — a normal setup —
// report identical working directories, so this always declines and tier 3
// raises whatever Ghostty window was last frontmost. The file still opens in
// the correct nvim; the user may just be looking at the wrong surface. There is
// no cheap fix: the sdef exposes only id, name (title) and working directory
// for a terminal, and none of those carries the child pid that would tie a
// surface to a specific nvim. Reporting is kept honest instead — Focusable()
// answers "app", not "heuristic", when a launch_cwd is shared.
func (c *Client) FocusByWorkingDir(ctx context.Context, launchCwd string) error {
	if launchCwd == "" {
		return errors.New("no launch_cwd recorded")
	}
	recs, err := c.listTerminals(ctx)
	if err != nil {
		return err
	}
	want := resolve.Canonical(launchCwd)
	var hits []terminalRecord
	for _, r := range recs {
		if r.Cwd == "" {
			continue
		}
		if resolve.Canonical(r.Cwd) == want {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		return fmt.Errorf("%d terminals match %s, need exactly 1", len(hits), launchCwd)
	}
	return c.FocusTerminalID(ctx, hits[0].ID)
}

// ActivateApp raises Ghostty without targeting a surface (tier 3). It uses
// `open -a`, which needs no Automation permission and therefore always works.
func (c *Client) ActivateApp(ctx context.Context) error {
	return exec.CommandContext(ctx, "open", "-a", appPath).Run()
}

// Focus walks the three tiers and reports which one succeeded.
func (c *Client) Focus(ctx context.Context, spawnID, launchCwd string) string {
	if id, ok := c.TerminalIDFor(spawnID); ok {
		if err := c.FocusTerminalID(ctx, id); err == nil {
			return FocusExact
		}
	}
	if err := c.FocusByWorkingDir(ctx, launchCwd); err == nil {
		return FocusHeuristic
	}
	_ = c.ActivateApp(ctx)
	return FocusApp
}

// Focusable reports the best tier available for an instance WITHOUT performing
// any AppleScript call, so /resolve stays fast and never trips a TCC prompt on
// a mere hover.
//
// launchCwdUnique must be true only when no other live instance shares this
// one's launch_cwd. Without that check the answer would be "heuristic" for
// every instance the daemon did not spawn, which is a promise tier 2 cannot
// keep: it focuses only when EXACTLY ONE terminal matches, so two nvims in the
// same worktree always degrade to tier 3.
func (c *Client) Focusable(spawnID, launchCwd string, launchCwdUnique bool) string {
	if _, ok := c.TerminalIDFor(spawnID); ok {
		return FocusExact
	}
	if launchCwd != "" && launchCwdUnique {
		return FocusHeuristic
	}
	return FocusApp
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, strings.TrimSpace(l))
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
