// Package httpapi serves the loopback HTTP API the browser extension talks to.
//
// Threat model. The daemon can launch an editor with a caller-chosen working
// directory, and a user's nvim config may set opt.exrc = true, so nvim looks
// there for a project-local .nvim.lua. Since 0.9 nvim sources one only if it is
// on the |trust| list, keyed on a hash of its contents, and prompts otherwise —
// so an attacker-chosen cwd is not arbitrary code execution. What a caller who
// picks the cwd does get is the choice of which already-trusted project config
// runs, and of where the user's editor is rooted. (0.12 additionally searches
// every parent directory, widening the first of those. It does not apply on the
// 0.11 floor this project supports, so nothing below rests on it.) Four
// controls follow, and none of them is cosmetic:
//
//  1. the listener is bound to 127.0.0.1 explicitly (never 0.0.0.0, and
//     deliberately no ::1 listener);
//  2. every request must present a shared secret, so a page in the browser
//     cannot drive the daemon even from localhost;
//  3. a request must be addressed to that same loopback host, so a rebound DNS
//     name cannot reach 1 and 2 by turning itself into a same-origin caller;
//  4. spawn targets must be one of the roots the daemon itself enumerated from
//     providers.json, and every resolved path must sit inside such a root.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codelink/internal/ghostty"
	"codelink/internal/nvimrpc"
	"codelink/internal/providers"
	"codelink/internal/registry"
	"codelink/internal/resolve"
	"codelink/internal/roots"
)

// Error codes returned to the extension.
const (
	CodeInstanceGone   = "INSTANCE_GONE"
	CodeFileNotFound   = "FILE_NOT_FOUND"
	CodeRootNotAllowed = "ROOT_NOT_ALLOWED"
	CodeNvimError      = "NVIM_ERROR"
	CodeSpawnFailed    = "SPAWN_FAILED"
	CodeSpawnTimeout   = "SPAWN_TIMEOUT"
	CodeBadRequest     = "BAD_REQUEST"
	CodeNoProvider     = "NO_PROVIDER"
)

// Kinds of link target.
const (
	KindFile        = "file"
	KindDirectory   = "directory"
	KindUnsupported = "unsupported"
)

// spawnPollBudget bounds the wait for a freshly spawned nvim to register.
//
// This is a ceiling, not a delay: the poll below runs every 25ms and returns as
// soon as the entry appears. It has to be generous because the clock starts
// before Ghostty has even created the window — a bare interactive nvim was
// measured at 281ms to register, and the real config is lazy.nvim with 33
// plugin specs on top of window creation, surface allocation and exec.
const spawnPollBudget = 5 * time.Second

// spawnPollInterval keeps the common case fast within that ceiling.
const spawnPollInterval = 25 * time.Millisecond

// Options configures a Server.
type Options struct {
	ConfigPath  string
	StateDir    string
	InstanceDir string
	SockDir     string
	TokenPath   string
	TokenJSPath string
	NvimBin     string
	Port        int
	Version     string
}

// Server is the daemon's HTTP surface.
type Server struct {
	opts  Options
	token string
	start time.Time

	cfgMu sync.RWMutex
	cfg   *providers.Config

	reg   *registry.Registry
	roots *roots.Manager
	term  *ghostty.Client
}

// NewServer loads the config, establishes the token and prepares the sub-systems.
func NewServer(opts Options) (*Server, error) {
	cfg, err := providers.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	token, err := loadOrCreateToken(opts.TokenPath)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:  opts,
		token: token,
		start: time.Now(),
		cfg:   cfg,
		reg:   registry.New(opts.InstanceDir, opts.SockDir),
		roots: roots.NewManager(opts.StateDir),
		term:  ghostty.New(),
	}
	if opts.TokenJSPath != "" {
		if err := writeTokenJS(opts.TokenJSPath, token); err != nil {
			log.Printf("warning: could not write %s: %v", opts.TokenJSPath, err)
		}
	}
	return s, nil
}

// Config returns the currently loaded providers config.
func (s *Server) Config() *providers.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// Reload re-reads providers.json.
func (s *Server) Reload() error {
	cfg, err := providers.Load(s.opts.ConfigPath)
	if err != nil {
		return err
	}
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	return nil
}

// loadOrCreateToken reads the 32-byte hex shared secret, creating it on first
// start with 0600 permissions.
func loadOrCreateToken(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(raw)); tok != "" {
			return tok, nil
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func writeTokenJS(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("self.CODELINK_TOKEN = '%s';\n", token)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	// WriteFile applies its mode only when it CREATES the file, so a
	// token.gen.js already sitting in the checkout — left by an earlier build,
	// or created by anything with a laxer umask — keeps whatever permissions it
	// had, and the shared secret stays world-readable.
	return os.Chmod(path, 0o600)
}

// allowedOrigin is the single browser origin permitted to talk to the daemon.
func (s *Server) allowedOrigin() string {
	id := s.Config().ExtensionID
	if id == "" {
		return ""
	}
	return "chrome-extension://" + id
}

// Handler builds the routed, authenticated mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/resolve", s.handleResolve)
	mux.HandleFunc("/open", s.handleOpen)
	mux.HandleFunc("/instances", s.handleInstances)
	mux.HandleFunc("/reload", s.handleReload)
	return s.accessLog(s.guard(mux))
}

// accessLog records one line per request. Without it a "the button does nothing"
// report is undiagnosable: there is no way to tell a request that never arrived
// (extension not loaded, stale build, wrong port) from one that arrived and was
// rejected. Set CODELINK_QUIET=1 to silence it.
func (s *Server) accessLog(next http.Handler) http.Handler {
	if os.Getenv("CODELINK_QUIET") == "1" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("%s %s%s -> %d (%s) origin=%q",
			r.Method, r.URL.Path, querySuffix(r), rec.status,
			time.Since(start).Round(time.Millisecond), r.Header.Get("Origin"))
	})
}

// querySuffix keeps the log readable: the url= parameter is the whole point of a
// /resolve call, but the rest is noise.
func querySuffix(r *http.Request) string {
	if u := r.URL.Query().Get("url"); u != "" {
		return "?url=" + u
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// guard enforces the Host allowlist, CORS and authentication.
//
// The CORS policy IS the anti-CSRF mechanism. Only the extension's own origin
// is ever echoed back; any other web page — including one on a host the
// providers config matches — must receive 403 with no Access-Control-* headers
// at all, so the browser refuses to hand it any response body even though the
// daemon is reachable on loopback.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostOK(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		allowed := s.allowedOrigin()
		originOK := allowed != "" && origin == allowed

		if r.Method == http.MethodOptions {
			// Preflights cannot carry custom headers, so they are answered
			// purely on Origin.
			if !originOK {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", allowed)
			h.Set("Access-Control-Allow-Headers", "X-Codelink-Client, X-Codelink-Token, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			h.Set("Vary", "Origin")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Header.Get("X-Codelink-Client") != "ext" || !s.tokenOK(r.Header.Get("X-Codelink-Token")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if originOK {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}

// hostOK reports whether the request addressed the daemon by the loopback name
// it actually listens on.
//
// This is the anti-DNS-rebinding control. A page on attacker.example whose name
// is re-pointed at 127.0.0.1 becomes SAME-ORIGIN with the daemon's port: the
// browser then sends no preflight, no Origin header, and allows arbitrary
// custom headers — so the CORS policy above never even runs. The one thing such
// a request cannot forge is the Host header, which still carries the attacker's
// name. The token remains the real barrier; this just removes a whole class of
// attempt before it reaches it.
//
// A Host without a port is rejected too: the extension always talks to
// 127.0.0.1:<port>, and no rebinding case needs that shape accepted.
func (s *Server) hostOK(host string) bool {
	h, port, err := net.SplitHostPort(host)
	if err != nil || port != strconv.Itoa(s.opts.Port) {
		return false
	}
	// No ::1 here on purpose, matching Serve: exactly one address is listened
	// on, so exactly one is accepted.
	return h == "127.0.0.1" || h == "localhost"
}

func (s *Server) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ---------------------------------------------------------------- /health

type healthResponse struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	Instances int    `json:"instances"`
	Providers int    `json:"providers"`
	UptimeS   int64  `json:"uptime_s"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	list, _ := s.reg.List()
	writeJSON(w, http.StatusOK, healthResponse{
		OK:        true,
		Version:   s.opts.Version,
		PID:       os.Getpid(),
		Instances: len(list),
		Providers: len(s.Config().Providers),
		UptimeS:   int64(time.Since(s.start).Seconds()),
	})
}

// --------------------------------------------------------------- /resolve

// OpenInstance is a running nvim that can open the requested file.
type OpenInstance struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Root        string `json:"root"`
	Cwd         string `json:"cwd"`
	LocalPath   string `json:"localPath"`
	InProject   bool   `json:"inProject"`
	LastFocused int64  `json:"lastFocused"`
	Focusable   string `json:"focusable"`
}

type resolveResponse struct {
	OK             bool              `json:"ok"`
	Code           string            `json:"code,omitempty"`
	Parsed         *providers.Parsed `json:"parsed,omitempty"`
	OpenInstances  []OpenInstance    `json:"openInstances"`
	RootCandidates []roots.Candidate `json:"rootCandidates"`
	Warnings       []string          `json:"warnings"`
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, http.StatusOK, resolveResponse{
			OK: false, Code: CodeBadRequest,
			OpenInstances: []OpenInstance{}, RootCandidates: []roots.Candidate{},
			Warnings: []string{"missing url parameter"},
		})
		return
	}
	out := s.resolveURL(raw)
	writeJSON(w, http.StatusOK, out)
}

// resolveURL is the shared body of /resolve, also used by `codelink doctor`.
func (s *Server) resolveURL(raw string) resolveResponse {
	empty := resolveResponse{
		OpenInstances:  []OpenInstance{},
		RootCandidates: []roots.Candidate{},
		Warnings:       []string{},
	}
	cfg := s.Config()
	parsed, ok := cfg.Parse(raw)
	if !ok {
		empty.OK = false
		empty.Code = CodeNoProvider
		return empty
	}
	prov := parsed.Owner()
	parsed.Project, parsed.ProjectName = resolve.Project(parsed.RepoPath, prov.ProjectMarkers)

	resp := empty
	resp.OK = true
	resp.Parsed = parsed

	if parsed.Ref != nil && !parsed.RefIsDefault {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"%s=%s — pinned revision; local file may differ", prov.RefParam, *parsed.Ref))
	}

	// A URL that names its repository may only ever reach that repository's
	// checkout. The scope is inert when the provider captured no repo group, so
	// such providers keep probing every root.
	scope := s.roots.ScopeFor(prov, parsed.Repo)
	allRoots := scope.Filter(s.roots.Expand(prov))

	// Instances first: an nvim that can already open the file is the best
	// possible answer.
	instances, _ := s.reg.List()

	// Tier-2 focusing needs a UNIQUE working directory to aim at, so count how
	// many live instances share each launch_cwd before reporting focusability.
	launchCwdCount := map[string]int{}
	for _, inst := range instances {
		if inst.LaunchCwd != "" {
			launchCwdCount[resolve.Canonical(inst.LaunchCwd)]++
		}
	}

	openInstances := make([]OpenInstance, 0, len(instances))
	rootsWithInstance := map[string]bool{}
	for _, inst := range instances {
		// The scope applies here too, and this is where it matters most: an nvim
		// already sitting in the wrong checkout is the TOP-ranked answer, so
		// filtering only the root candidates would still open the wrong file.
		if !scope.Allows(inst.Root) {
			continue
		}
		local, _, ok := resolve.Resolve(parsed.RepoPath, inst.Cwd)
		if !ok {
			continue
		}
		if !roots.PathAllowed(allRoots, local) {
			continue
		}
		rootsWithInstance[resolve.Canonical(inst.Root)] = true
		openInstances = append(openInstances, OpenInstance{
			ID:        inst.ID(),
			Label:     inst.Label,
			Root:      inst.Root,
			Cwd:       inst.Cwd,
			LocalPath: local,
			// inProject is a RANKING signal only. Filtering on it would wrongly
			// exclude an nvim sitting at the repo root, which can open the file
			// perfectly well.
			InProject:   inProject(inst.Root, parsed.Project, inst.Cwd),
			LastFocused: inst.LastFocused,
			Focusable:   s.term.Focusable(inst.Spawn(), inst.LaunchCwd, launchCwdCount[resolve.Canonical(inst.LaunchCwd)] == 1),
		})
	}
	sort.SliceStable(openInstances, func(a, b int) bool {
		x, y := openInstances[a], openInstances[b]
		if x.InProject != y.InProject {
			return x.InProject
		}
		return x.LastFocused > y.LastFocused
	})
	resp.OpenInstances = openInstances

	cands := s.roots.Probe(allRoots, parsed.RepoPath)
	for i := range cands {
		cands[i].HasOpenInstance = rootsWithInstance[resolve.Canonical(cands[i].Root)]
	}
	s.roots.Sort(cands)
	resp.RootCandidates = cands

	// Naming the repo that has no checkout here is the whole diagnosis; the
	// generic "no local checkout contains this path" below would send the user
	// looking for a missing file instead.
	//
	// The gate is the FILTERED root list, not the empty result: a checkout that
	// survived the filter but does not have this file yet is a missing file, not
	// a missing checkout, and saying otherwise sends the reader after the wrong
	// thing. With no surviving root there is nothing to probe and no instance
	// that could pass, so both lists are empty by construction.
	if scope.Active() && len(allRoots) == 0 {
		resp.Warnings = append(resp.Warnings,
			fmt.Sprintf("repo %q matched no local checkout", parsed.Repo))
	}

	// kind comes from stat'ing the first path that actually resolved locally,
	// never from guessing at the URL shape.
	first := ""
	if len(openInstances) > 0 {
		first = openInstances[0].LocalPath
	} else if len(cands) > 0 {
		first = cands[0].LocalPath
	}
	switch {
	case first == "":
		resp.Parsed.Kind = KindUnsupported
		resp.OpenInstances = []OpenInstance{}
		resp.RootCandidates = []roots.Candidate{}
		resp.Warnings = append(resp.Warnings, "no local checkout contains this path")
	default:
		fi, err := os.Stat(first)
		switch {
		case err != nil:
			resp.Parsed.Kind = KindUnsupported
		case fi.IsDir():
			resp.Parsed.Kind = KindDirectory
		default:
			resp.Parsed.Kind = KindFile
		}
	}
	return resp
}

// inProject reports whether cwd sits at or below <root>/<project>.
func inProject(root, project, cwd string) bool {
	if root == "" || project == "" || cwd == "" {
		return false
	}
	return resolve.Within(resolve.Canonical(filepath.Join(root, filepath.FromSlash(project))), resolve.Canonical(cwd))
}

// ------------------------------------------------------------------ /open

type openRequest struct {
	Mode     string `json:"mode"`
	Target   string `json:"target"`
	RepoPath string `json:"repoPath"`
	Line     *int   `json:"line"`
	EndLine  *int   `json:"endLine"`
	Focus    *bool  `json:"focus"`
}

type openResponse struct {
	OK       bool   `json:"ok"`
	Instance string `json:"instance,omitempty"`
	Focused  string `json:"focused,omitempty"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
}

func fail(code, format string, args ...any) openResponse {
	return openResponse{OK: false, Code: code, Error: fmt.Sprintf(format, args...)}
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, fail(CodeBadRequest, "POST required"))
		return
	}
	var req openRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, fail(CodeBadRequest, "malformed JSON: %v", err))
		return
	}
	if req.RepoPath == "" {
		writeJSON(w, http.StatusOK, fail(CodeBadRequest, "repoPath is required"))
		return
	}
	writeJSON(w, http.StatusOK, s.open(r.Context(), req))
}

func (s *Server) open(ctx context.Context, req openRequest) openResponse {
	cfg := s.Config()
	if len(cfg.Providers) == 0 {
		return fail(CodeNoProvider, "no providers configured")
	}
	allRoots := s.roots.ExpandAll(cfg)

	switch req.Mode {
	case "existing":
		return s.openExisting(ctx, req, allRoots)
	case "new":
		return s.openNew(ctx, req, allRoots)
	default:
		return fail(CodeBadRequest, "unknown mode %q", req.Mode)
	}
}

// openExisting drives an already-running nvim.
func (s *Server) openExisting(ctx context.Context, req openRequest, allRoots []roots.Root) openResponse {
	instances, _ := s.reg.List()
	var target *registry.Instance
	var others []*registry.Instance
	for _, i := range instances {
		if i.ID() == req.Target {
			target = i
		} else {
			others = append(others, i)
		}
	}
	if target == nil {
		return fail(CodeInstanceGone, "no live instance with id %q", req.Target)
	}

	// The named instance is tried first; the rest are fallbacks for the case
	// where its socket turns out to be dead (E247).
	candidates := append([]*registry.Instance{target}, others...)

	var lastErr error
	for _, inst := range candidates {
		local, _, ok := resolve.Resolve(req.RepoPath, inst.Cwd)
		if !ok {
			continue
		}
		if !roots.PathAllowed(allRoots, local) {
			// Skip this candidate rather than aborting: one instance sitting
			// outside the allowlist must not prevent the E247 fallback chain
			// from reaching a perfectly good later candidate.
			lastErr = fmt.Errorf("resolved path is outside every configured root: %s", local)
			continue
		}
		resp, err := nvimrpc.Open(ctx, s.opts.NvimBin, inst.Socket(), nvimrpc.Request{
			Path: local, Line: req.Line, EndLine: req.EndLine,
		})
		if err != nil {
			lastErr = err
			if errors.Is(err, nvimrpc.ErrDeadSocket) {
				s.reg.Prune(inst)
				continue
			}
			return fail(CodeNvimError, "%v", err)
		}
		if !resp.OK {
			return fail(CodeNvimError, "%s", resp.Error)
		}
		s.roots.TouchRecent(inst.Root)
		return openResponse{OK: true, Instance: inst.ID(), Focused: s.maybeFocus(ctx, req, inst)}
	}
	if lastErr != nil {
		return fail(CodeInstanceGone, "every candidate instance was unreachable: %v", lastErr)
	}
	return fail(CodeFileNotFound, "no live instance can reach %s", req.RepoPath)
}

// openNew spawns a fresh nvim in a Ghostty window.
func (s *Server) openNew(ctx context.Context, req openRequest, allRoots []roots.Root) openResponse {
	// SECURITY: the spawn target must be one of the roots the daemon itself
	// enumerated. Because nvim runs with opt.exrc = true, accepting an
	// arbitrary directory here would hand a caller the choice of which trusted
	// .nvim.lua — that directory's or any parent's — is sourced, and of where
	// the user's editor lands. The trust list stops an unknown file from
	// running silently; it does not make the cwd the caller's to pick.
	root, ok := roots.Allowed(allRoots, req.Target)
	if !ok {
		return fail(CodeRootNotAllowed, "%q is not one of the configured roots", req.Target)
	}
	local, _, ok := resolve.Resolve(req.RepoPath, root.Path)
	if !ok {
		return fail(CodeFileNotFound, "%s does not exist under %s", req.RepoPath, root.Path)
	}
	if !roots.PathAllowed(allRoots, local) {
		return fail(CodeRootNotAllowed, "resolved path is outside every configured root: %s", local)
	}

	workdir := s.spawnWorkdir(root, req.RepoPath, allRoots)

	spawnID, err := ghostty.NewSpawnID()
	if err != nil {
		return fail(CodeSpawnFailed, "%v", err)
	}
	if _, err := s.term.Spawn(ctx, workdir, s.opts.NvimBin, spawnID); err != nil {
		return fail(CodeSpawnFailed, "%v", err)
	}

	// Poll for the new instance to announce itself, then drive it through the
	// ordinary RPC path — that reuses one code path and gets range selection
	// and the flash highlight for free.
	inst := s.waitForSpawn(ctx, spawnID)
	if inst == nil {
		// The window exists but we never got a socket to talk to, so the file
		// was NOT delivered. Reporting ok:true here would show the user a green
		// checkmark over an empty nvim.
		s.roots.TouchRecent(root.Path)
		return fail(CodeSpawnTimeout,
			"opened a window in %s, but the new nvim did not register within %s, so %s could not be opened",
			workdir, spawnPollBudget, req.RepoPath)
	}
	resp, err := nvimrpc.Open(ctx, s.opts.NvimBin, inst.Socket(), nvimrpc.Request{
		Path: local, Line: req.Line, EndLine: req.EndLine,
	})
	if err != nil {
		return fail(CodeNvimError, "%v", err)
	}
	if !resp.OK {
		return fail(CodeNvimError, "%s", resp.Error)
	}
	s.roots.TouchRecent(root.Path)
	return openResponse{OK: true, Instance: inst.ID(), Focused: s.maybeFocus(ctx, req, inst)}
}

// spawnWorkdir picks the directory the new window opens in: the project
// directory when we know it, so the shell and nvim start somewhere useful, and
// the root itself otherwise.
//
// SECURITY: this cwd is where nvim begins its exrc search, which walks upwards
// from here, so it needs the same physical containment check as the resolved
// file — containment is what keeps that upward walk on known ground. The project
// component comes from the caller's repoPath and filepath.Join cleans it only
// LEXICALLY, which a directory symlink inside the root pointing out of it
// survives untouched. The check already performed on the resolved file does not
// cover this: a file behind such a symlink can canonicalise back INTO the root
// and pass, while the cwd it was reached through still sits outside. Failing the
// check is not a request error — falling back to root.Path, which is allowed by
// construction, loses nothing but a nicer starting directory.
func (s *Server) spawnWorkdir(root roots.Root, repoPath string, allRoots []roots.Root) string {
	project, _ := resolve.Project(repoPath, s.projectMarkers())
	if project == "" {
		return root.Path
	}
	cand := filepath.Join(root.Path, filepath.FromSlash(project))
	if fi, err := os.Stat(cand); err != nil || !fi.IsDir() {
		return root.Path
	}
	if !roots.PathAllowed(allRoots, cand) {
		return root.Path
	}
	return cand
}

func (s *Server) waitForSpawn(ctx context.Context, spawnID string) *registry.Instance {
	deadline := time.Now().Add(spawnPollBudget)
	for time.Now().Before(deadline) {
		if inst := s.reg.BySpawnID(spawnID); inst != nil && inst.Socket() != "" {
			return inst
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(spawnPollInterval):
		}
	}
	return s.reg.BySpawnID(spawnID)
}

func (s *Server) maybeFocus(ctx context.Context, req openRequest, inst *registry.Instance) string {
	if req.Focus != nil && !*req.Focus {
		return ghostty.FocusNone
	}
	return s.term.Focus(ctx, inst.Spawn(), inst.LaunchCwd)
}

// projectMarkers is the union of every provider's markers. /open carries only a
// repoPath, not the originating URL, so the owning provider is unknown here;
// the union is the honest superset and the markers are generic directory names
// anyway.
func (s *Server) projectMarkers() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.Config().Providers {
		for _, m := range p.ProjectMarkers {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// ------------------------------------------------- /instances and /reload

func (s *Server) handleInstances(w http.ResponseWriter, _ *http.Request) {
	list, pruned := s.reg.List()
	if list == nil {
		list = []*registry.Instance{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"instances": list,
		"pruned":    pruned,
	})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "code": CodeBadRequest, "error": "POST required"})
		return
	}
	if err := s.Reload(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg := s.Config()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": len(cfg.Providers)})
}

// ----------------------------------------------------------------- serve

// Serve binds the loopback listener and runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	// Explicitly 127.0.0.1: binding 0.0.0.0 would expose an editor-spawning
	// API to the network, and no ::1 listener is created on purpose so there
	// is exactly one reachable address to reason about.
	addr := fmt.Sprintf("127.0.0.1:%d", s.opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Warm one probe per root so the first real hover meets a warm FUSE mount
	// instead of paying the ~500ms cold stat.
	go s.roots.Warm(s.roots.ExpandAll(s.Config()))

	log.Printf("codelink %s listening on http://%s (pid %d)", s.opts.Version, addr, os.Getpid())
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ResolveForDoctor exposes a resolve result to the doctor subcommand.
func (s *Server) ResolveForDoctor(raw string) (any, bool) {
	out := s.resolveURL(raw)
	return out, out.OK
}
