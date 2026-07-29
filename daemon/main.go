// Command codelink is a loopback daemon that turns a code-hosting URL into an
// open buffer in a local Neovim.
//
// Subcommands:
//
//	codelink serve            run the HTTP API on 127.0.0.1
//	codelink build-manifest   generate the browser extension's manifest.json
//	codelink doctor           dump resolved providers, roots and instances
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"codelink/internal/httpapi"
	"codelink/internal/providers"
	"codelink/internal/registry"
	"codelink/internal/roots"
)

// version is reported by /health and `codelink doctor`.
const version = "0.1.0"

const (
	defaultPort    = 47391
	defaultNvimBin = "/opt/homebrew/bin/nvim"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe()
	case "build-manifest":
		err = cmdBuildManifest()
	case "doctor":
		err = cmdDoctor()
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "codelink: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "codelink: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `codelink `+version+`

usage:
  codelink serve            run the HTTP API on 127.0.0.1:`+strconv.Itoa(defaultPort)+`
  codelink build-manifest   generate the extension manifest.json + hosts.gen.js
  codelink doctor           dump providers, roots, instances, token and port

environment:
  CODELINK_PROVIDERS   path to providers.json (default `+providers.DefaultConfigPath+`)
  CODELINK_PORT        listen port (default `+strconv.Itoa(defaultPort)+`)
  CODELINK_NVIM        nvim binary (default `+defaultNvimBin+`)
`)
}

// ------------------------------------------------------------------ paths

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func configPath() string {
	if p := os.Getenv("CODELINK_PROVIDERS"); p != "" {
		return roots.ExpandTilde(p)
	}
	return roots.ExpandTilde(providers.DefaultConfigPath)
}

func stateDir() string    { return filepath.Join(home(), ".local", "state", "codelink") }
func instanceDir() string { return filepath.Join(stateDir(), "instances") }
func sockDir() string     { return filepath.Join(stateDir(), "sock") }
func tokenPath() string   { return filepath.Join(stateDir(), "token") }

func extensionDir() string {
	return filepath.Join(home(), ".config", "codelink", "extension")
}

func port() int {
	if v := os.Getenv("CODELINK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return defaultPort
}

func nvimBin() string {
	if v := os.Getenv("CODELINK_NVIM"); v != "" {
		return v
	}
	return defaultNvimBin
}

func serverOptions() httpapi.Options {
	return httpapi.Options{
		ConfigPath:  configPath(),
		StateDir:    stateDir(),
		InstanceDir: instanceDir(),
		SockDir:     sockDir(),
		TokenPath:   tokenPath(),
		TokenJSPath: filepath.Join(extensionDir(), "token.gen.js"),
		NvimBin:     nvimBin(),
		Port:        port(),
		Version:     version,
	}
}

// ------------------------------------------------------------------ serve

func cmdServe() error {
	for _, d := range []string{instanceDir(), sockDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	srv, err := httpapi.NewServer(serverOptions())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx)
}

// --------------------------------------------------------- build-manifest

// matchesToken is the literal placeholder in the manifest template. A
// one-element array ["__MATCHES__"] is replaced by the real array, so the
// template stays valid JSON and can be linted on its own.
var matchesArrayRe = regexp.MustCompile(`\[\s*"__MATCHES__"\s*\]`)

// keyLineRe matches a whole "key": "__EXTENSION_KEY__" property including its
// trailing comma, so the property can be dropped cleanly when no key is
// available.
var keyLineRe = regexp.MustCompile(`(?m)^[ \t]*"[A-Za-z_]+"[ \t]*:[ \t]*"__EXTENSION_KEY__"[ \t]*,?[ \t]*\r?\n`)

func cmdBuildManifest() error {
	cfg, err := providers.Load(configPath())
	if err != nil {
		return err
	}

	dir := extensionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// hosts.gen.js is written unconditionally: the content script needs it for
	// cheap triage and it does not depend on the template.
	hostsJSON, err := json.Marshal(cfg.AllHosts())
	if err != nil {
		return err
	}
	hostsPath := filepath.Join(dir, "hosts.gen.js")
	hostsBody := fmt.Sprintf("self.CODELINK_HOSTS = %s;\n", hostsJSON)
	if err := writeIfChanged(hostsPath, []byte(hostsBody), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n  hosts: %s\n", hostsPath, hostsJSON)

	tmplPath := filepath.Join(dir, "manifest.template.json")
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			// The extension is authored separately and may not exist yet.
			// This is a normal state, not a failure.
			fmt.Printf("\nno manifest template at %s yet — skipping manifest.json.\n"+
				"Re-run `codelink build-manifest` once the extension provides it.\n", tmplPath)
			return nil
		}
		return err
	}

	/*
	 * Injection scope and link triage are deliberately DIFFERENT things.
	 *
	 * The content script runs everywhere by default, but only offers a button
	 * for links whose host matches a provider (hosts.gen.js drives that check,
	 * and triage() resolves the LINK's URL, never the page's). That is what
	 * makes codelink work on a page that merely *mentions* a repo link — a
	 * local HTML report opened over file://, a ticket, a chat log — none of
	 * which live on a provider host.
	 *
	 * Scoping injection to the provider hosts instead would silently do nothing
	 * on exactly those pages, with no button and no error to explain why.
	 *
	 * Note file:// pages additionally require the per-extension "Allow access
	 * to file URLs" toggle; no manifest setting can grant it.
	 *
	 * Override with an "inject" array in providers.json if a narrower scope is
	 * wanted.
	 */
	patterns := cfg.Inject
	if len(patterns) == 0 {
		patterns = []string{"<all_urls>"}
	}
	// json.Marshal HTML-escapes < and > into their \u00XX forms, so the match
	// pattern would land in the file as an escaped string rather than a literal
	// <all_urls>. Chromium's JSON parser decodes it correctly either way, but a
	// manifest that cannot be grepped for the pattern it actually uses is a trap
	// for the next reader, and for any validator that matches before decoding.
	// SetEscapeHTML(false) keeps it literal.
	var patternsBuf bytes.Buffer
	enc := json.NewEncoder(&patternsBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(patterns); err != nil {
		return err
	}
	patternsJSON := bytes.TrimSpace(patternsBuf.Bytes())

	out := matchesArrayRe.ReplaceAllLiteralString(string(raw), string(patternsJSON))
	// Also handle a bare "__MATCHES__" string not wrapped in an array.
	out = strings.ReplaceAll(out, `"__MATCHES__"`, string(patternsJSON))

	keyMissing := false
	if key := extensionKey(); key != "" {
		out = strings.ReplaceAll(out, "__EXTENSION_KEY__", key)
	} else if strings.Contains(out, "__EXTENSION_KEY__") {
		// No key material available: drop the property entirely rather than
		// emitting a manifest Chromium will reject outright.
		out = keyLineRe.ReplaceAllString(out, "")
		out = strings.ReplaceAll(out, "__EXTENSION_KEY__", "")
		keyMissing = true
	}

	// Fail loudly rather than writing a manifest Chromium cannot parse.
	var probe any
	if err := json.Unmarshal([]byte(out), &probe); err != nil {
		return fmt.Errorf("generated manifest is not valid JSON: %w", err)
	}

	manifestPath := filepath.Join(dir, "manifest.json")
	if err := writeIfChanged(manifestPath, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n  matches: %s\n", manifestPath, patternsJSON)

	if keyMissing {
		// Chromium derives an unpacked extension's id from "key" when present
		// and from the install path otherwise. The daemon only accepts
		// chrome-extension://<extensionId>, so a manifest without "key" loads
		// under some other id and every request is rejected with 403. Silently
		// emitting that manifest would look like a CORS bug at debug time.
		fmt.Printf("\nWARNING: no extension key material found, so the \"key\" property was dropped.\n"+
			"  Chromium will then derive the extension id from the install path, and it will NOT be\n"+
			"    %s\n"+
			"  which is the only origin `codelink serve` accepts. Requests would fail with 403.\n"+
			"  Supply the key via $CODELINK_EXTENSION_KEY or %s,\n"+
			"  or update extensionId in %s to the id Chromium actually assigns.\n",
			cfg.ExtensionID,
			filepath.Join(home(), ".local", "share", "codelink", "extension_key.txt"),
			configPath())
	}
	return nil
}

// extensionKey looks for optional key material for the manifest's "key" field.
func extensionKey() string {
	if v := strings.TrimSpace(os.Getenv("CODELINK_EXTENSION_KEY")); v != "" {
		return v
	}
	p := filepath.Join(home(), ".local", "share", "codelink", "extension_key.txt")
	if raw, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

// writeIfChanged keeps the command idempotent and avoids touching mtimes (and
// thus extension reloads) when nothing actually changed.
func writeIfChanged(path string, body []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(body) {
		return nil
	}
	return os.WriteFile(path, body, mode)
}

// ----------------------------------------------------------------- doctor

func cmdDoctor() error {
	cfgPath := configPath()
	fmt.Printf("codelink %s\n\n", version)

	fmt.Println("== config ==")
	fmt.Printf("  providers.json : %s\n", cfgPath)
	cfg, err := providers.Load(cfgPath)
	if err != nil {
		fmt.Printf("  STATUS         : FAILED TO LOAD — %v\n", err)
		return err
	}
	fmt.Printf("  extensionId    : %s\n", cfg.ExtensionID)
	fmt.Printf("  allowed origin : chrome-extension://%s\n", cfg.ExtensionID)
	fmt.Printf("  port           : %d (127.0.0.1 only)\n", port())
	fmt.Printf("  nvim           : %s%s\n", nvimBin(), existsNote(nvimBin()))
	fmt.Printf("  token          : %s%s\n", tokenPath(), existsNote(tokenPath()))
	fmt.Printf("  token.gen.js   : %s%s\n", filepath.Join(extensionDir(), "token.gen.js"), existsNote(filepath.Join(extensionDir(), "token.gen.js")))
	fmt.Printf("  state dir      : %s\n", stateDir())
	fmt.Printf("  instances dir  : %s%s\n", instanceDir(), existsNote(instanceDir()))
	fmt.Println()

	fmt.Println("== providers ==")
	for _, p := range cfg.Providers {
		fmt.Printf("  %s\n", p.ID)
		fmt.Printf("    hosts          : %s\n", strings.Join(p.Hosts, ", "))
		fmt.Printf("    match patterns : %s\n", strings.Join(p.MatchPatterns(), ", "))
		fmt.Printf("    refParam       : %s (default %q)\n", p.RefParam, p.DefaultRef)
		fmt.Printf("    projectMarkers : %s\n", strings.Join(p.ProjectMarkers, ", "))
		for i, m := range p.Match {
			hash := m.Hash
			if hash == "" {
				hash = p.Hash + "  (provider fallback)"
			}
			fmt.Printf("    match[%d].path  : %s\n", i, m.Path)
			fmt.Printf("    match[%d].hash  : %s\n", i, hash)
		}
	}
	fmt.Println()

	mgr := roots.NewManager(stateDir())
	fmt.Println("== roots ==")
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ROOT\tLABEL\tMOUNTED\tRECENCY\tAGE")
	var all []roots.Root
	for _, p := range cfg.Providers {
		all = append(all, mgr.Expand(p)...)
	}
	for _, r := range all {
		age := "-"
		rec := "-"
		if !r.RecencyTime.IsZero() {
			rec = r.RecencyTime.Format("2006-01-02 15:04:05")
			age = shortDuration(time.Since(r.RecencyTime))
		}
		label := r.Label
		if label == "" {
			label = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%v\t%s\t%s\n", r.Path, label, r.Mounted, rec, age)
	}
	_ = tw.Flush()
	if len(all) == 0 {
		fmt.Println("  (none — check the roots globs and whether the mounts are up)")
	}
	fmt.Println()

	fmt.Println("  skipped (declared but dropped):")
	reportSkipped(cfg, all)
	fmt.Println()

	fmt.Println("== recent (LRU) ==")
	recent := mgr.Recent()
	if len(recent) == 0 {
		fmt.Println("  (empty)")
	}
	for i, p := range recent {
		fmt.Printf("  %2d. %s\n", i+1, p)
	}
	fmt.Println()

	fmt.Println("== instances ==")
	reg := registry.New(instanceDir(), sockDir())
	list, pruned := reg.List()
	if len(list) == 0 {
		fmt.Println("  (no live nvim instances registered)")
	}
	itw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if len(list) > 0 {
		fmt.Fprintln(itw, "  ID\tLABEL\tCWD\tROOT\tSOCKET\tSPAWN\tLAST FOCUSED")
	}
	for _, i := range list {
		spawn := i.Spawn()
		if spawn == "" {
			spawn = "-"
		}
		lf := "-"
		if i.LastFocused > 0 {
			lf = shortDuration(time.Since(time.Unix(i.LastFocused, 0))) + " ago"
		}
		fmt.Fprintf(itw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			i.ID(), orDash(i.Label), orDash(i.Cwd), orDash(i.Root), orDash(i.Socket()), spawn, lf)
	}
	_ = itw.Flush()
	for _, p := range pruned {
		fmt.Printf("  pruned stale entry: %s\n", p)
	}
	fmt.Println()

	// A live end-to-end parse makes the whole chain observable in one command.
	if len(os.Args) > 2 {
		fmt.Println("== resolve ==")
		srv, err := httpapi.NewServer(serverOptions())
		if err != nil {
			return err
		}
		out, ok := srv.ResolveForDoctor(os.Args[2])
		raw, _ := json.MarshalIndent(out, "  ", "  ")
		fmt.Printf("  ok=%v\n  %s\n", ok, raw)
	} else {
		fmt.Println("tip: `codelink doctor <url>` also runs a full resolve of that URL.")
	}
	return nil
}

// reportSkipped re-globs the raw specs and prints the entries that Expand
// dropped, which is usually the interesting part when a hover finds nothing.
func reportSkipped(cfg *providers.Config, kept []roots.Root) {
	keptSet := map[string]bool{}
	for _, r := range kept {
		keptSet[r.Path] = true
	}
	any := false
	for _, p := range cfg.Providers {
		for _, spec := range p.Roots {
			var cands []string
			if spec.Glob != "" {
				m, _ := filepath.Glob(roots.ExpandTilde(spec.Glob))
				cands = m
			} else if spec.Path != "" {
				cands = []string{roots.ExpandTilde(spec.Path)}
			}
			for _, c := range cands {
				c = filepath.Clean(c)
				if keptSet[c] {
					continue
				}
				base := filepath.Base(c)
				reason := "not a directory"
				switch {
				case strings.HasPrefix(base, "."):
					reason = "dotfile, skipped by the glob filter"
				default:
					if fi, err := os.Lstat(c); err == nil && fi.IsDir() && spec.RequireMount {
						reason = "requireMount: not present in mount(8) — stale/unmounted"
					}
				}
				fmt.Printf("    %s  (%s)\n", c, reason)
				any = true
			}
		}
	}
	if !any {
		fmt.Println("    (none)")
	}
}

func existsNote(p string) string {
	if _, err := os.Stat(p); err == nil {
		return ""
	}
	return "   [MISSING]"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
