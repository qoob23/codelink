// Package registry reads the per-nvim-instance JSON files written by the
// Neovim side of codelink and prunes the ones that are no longer live.
//
// The directory is re-read on every request on purpose: it holds a handful of
// small files on local disk, so a full scan is sub-millisecond and buys us
// correctness without an fsnotify dependency.
package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// maxEntrySize caps how much of a registry entry is ever read. The Neovim side
// writes a few hundred bytes; a megabyte of it is not one of ours.
const maxEntrySize = 64 << 10

// Instance is one live (or recently dead) Neovim instance.
type Instance struct {
	V              int     `json:"v"`
	PID            int     `json:"pid"`
	Servername     *string `json:"servername"`
	AutoServername *string `json:"auto_servername"`
	Cwd            string  `json:"cwd"`
	LaunchCwd      string  `json:"launch_cwd"`
	Root           string  `json:"root"`
	Label          string  `json:"label"`
	SpawnID        *string `json:"spawn_id"`
	StartedAt      int64   `json:"started_at"`
	LastFocused    int64   `json:"last_focused"`

	// File is the registry entry this instance was decoded from.
	File string `json:"-"`
}

// ID is the stable identifier exposed over HTTP.
func (i *Instance) ID() string { return strconv.Itoa(i.PID) }

// Socket returns the first declared servername that actually exists on disk.
// servername may legitimately be absent or null, in which case the
// auto-generated one is used.
func (i *Instance) Socket() string {
	for _, cand := range []*string{i.Servername, i.AutoServername} {
		if cand == nil || *cand == "" {
			continue
		}
		if _, err := os.Stat(*cand); err == nil {
			return *cand
		}
	}
	return ""
}

// Spawn returns the spawn id, tolerating the JSON null the Neovim side writes
// when the instance was not spawned by codelink.
func (i *Instance) Spawn() string {
	if i.SpawnID == nil {
		return ""
	}
	return *i.SpawnID
}

// Registry is a view over the instances directory.
type Registry struct {
	Dir string
	// SockDir is the only directory this package will ever unlink a socket
	// from; it bounds the blast radius of orphan cleanup.
	SockDir string
}

// New returns a registry backed by dir, cleaning orphaned sockets in sockDir.
func New(dir, sockDir string) *Registry {
	return &Registry{Dir: dir, SockDir: sockDir}
}

// removeOwnedSocket unlinks a dead instance's servername socket.
//
// nvim does not remove its listen socket when it dies — a SIGKILL leaves
// sock/<pid>.sock behind forever — so the daemon collects it alongside the
// registry entry. Only paths directly inside SockDir are touched:
// auto_servername lives under $TMPDIR and belongs to nvim's own lifecycle, so
// unlinking that would be meddling with another process's state.
func (r *Registry) removeOwnedSocket(inst *Instance) {
	if inst == nil || r.SockDir == "" || inst.Servername == nil {
		return
	}
	sock := strings.TrimSpace(*inst.Servername)
	if sock == "" {
		return
	}
	if filepath.Clean(filepath.Dir(sock)) != filepath.Clean(r.SockDir) {
		return
	}
	_ = os.Remove(sock)
}

// processAlive reports whether pid exists. EPERM means the process exists but
// belongs to someone else, which still counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err != syscall.ESRCH
}

// List re-reads the directory and returns the live instances, unlinking every
// entry that fails to decode, whose process is gone, or whose sockets have all
// disappeared.
func (r *Registry) List() ([]*Instance, []string) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil, nil
	}
	var out []*Instance
	var pruned []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(r.Dir, e.Name())
		// Only ever open a plain file, and only a small one. List() runs on
		// every request, so a FIFO dropped in here as hang.json would block the
		// ReadFile until someone writes to it and wedge every endpoint at once;
		// a device node or a huge file is the same denial in a slower form.
		// Such an entry is left on disk rather than pruned — it is not ours to
		// delete, and deleting the thing we refused to read is how a bug like
		// this turns into data loss.
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxEntrySize {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var inst Instance
		if err := json.Unmarshal(raw, &inst); err != nil {
			// Undecodable: there is no servername to trust, so only the entry
			// itself is removed.
			prune(p, &pruned)
			continue
		}
		inst.File = p
		if !processAlive(inst.PID) {
			r.removeOwnedSocket(&inst)
			prune(p, &pruned)
			continue
		}
		if inst.Socket() == "" {
			prune(p, &pruned)
			continue
		}
		out = append(out, &inst)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].LastFocused != out[b].LastFocused {
			return out[a].LastFocused > out[b].LastFocused
		}
		return out[a].PID < out[b].PID
	})
	return out, pruned
}

// Get returns the live instance with the given id.
func (r *Registry) Get(id string) *Instance {
	list, _ := r.List()
	for _, i := range list {
		if i.ID() == id {
			return i
		}
	}
	return nil
}

// BySpawnID returns the live instance carrying the given spawn id.
func (r *Registry) BySpawnID(spawnID string) *Instance {
	if spawnID == "" {
		return nil
	}
	list, _ := r.List()
	for _, i := range list {
		if i.Spawn() == spawnID {
			return i
		}
	}
	return nil
}

// Prune removes a single registry entry and its orphaned socket, e.g. after
// nvim reported E247 — which means nothing is listening on that socket any more.
func (r *Registry) Prune(inst *Instance) {
	if inst == nil || inst.File == "" {
		return
	}
	r.removeOwnedSocket(inst)
	_ = os.Remove(inst.File)
}

func prune(path string, pruned *[]string) {
	if err := os.Remove(path); err == nil {
		*pruned = append(*pruned, path)
	}
}
