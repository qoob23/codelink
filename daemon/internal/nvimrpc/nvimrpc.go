// Package nvimrpc talks to a running Neovim over its socket.
//
// The transport is `nvim --server <sock> --remote-expr luaeval(...)`. exec is
// given an argv slice, so no shell is involved anywhere and shell quoting never
// enters the picture; the only escaping needed is vimscript's single-quote
// doubling inside the expression string. Measured at ~10ms, and it round-trips
// UTF-8, spaces and both quote characters.
package nvimrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds one RPC round trip.
const Timeout = 3 * time.Second

// ErrDeadSocket reports that nvim could not connect to the socket (E247), which
// means the registry entry is stale and should be pruned.
var ErrDeadSocket = errors.New("nvim socket is dead")

// Request is the payload handed to the Lua side. The field names are
// snake_case because that is the Lua-side contract, unlike the daemon's own
// camelCase HTTP API.
type Request struct {
	Path    string `json:"path"`
	Line    *int   `json:"line,omitempty"`
	EndLine *int   `json:"end_line,omitempty"`
}

// Response is what _G.__codelink_rpc returns.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Open asks the nvim listening on sock to open req.
//
// The Neovim side always exits 0 and answers application-level failures in the
// JSON body, so a non-zero exit status genuinely means a transport problem
// (dead socket, missing binary) and never a business error.
func Open(ctx context.Context, nvimBin, sock string, req Request) (Response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	// vimscript single-quoted strings escape ' by doubling it.
	escaped := strings.ReplaceAll(string(payload), "'", "''")
	expr := fmt.Sprintf(`luaeval('_G.__codelink_rpc(_A)', '%s')`, escaped)

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, nvimBin, "--server", sock, "--remote-expr", expr)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := string(ee.Stderr)
			if strings.Contains(stderr, "E247") {
				return Response{}, fmt.Errorf("%w: %s", ErrDeadSocket, firstLine(stderr))
			}
			if msg := firstLine(stderr); msg != "" {
				return Response{}, errors.New(msg)
			}
		}
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("nvim rpc timed out after %s", Timeout)
		}
		return Response{}, err
	}

	// --remote-expr prints the returned Lua/vim value; ours is a JSON string.
	body := strings.TrimSpace(string(out))
	var resp Response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return Response{}, fmt.Errorf("unreadable nvim response %q: %w", truncate(body, 200), err)
	}
	return resp, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
