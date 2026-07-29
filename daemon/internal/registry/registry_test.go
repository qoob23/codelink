package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// env is a throwaway instances+sock directory pair.
type env struct {
	dir  string
	sock string
	reg  *Registry
}

func newEnv(t *testing.T) env {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "instances")
	sock := filepath.Join(base, "sock")
	for _, d := range []string{dir, sock} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return env{dir: dir, sock: sock, reg: New(dir, sock)}
}

// write creates a registry entry plus (optionally) its socket file.
func (e env) write(t *testing.T, name string, inst Instance, makeSock bool) string {
	t.Helper()
	raw, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.dir, name+".json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if makeSock && inst.Servername != nil {
		if err := os.WriteFile(*inst.Servername, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// deadPID is a pid that cannot be running. Kill(pid,0) returns ESRCH for it.
const deadPID = 0x7FFFFFF0

func TestListPrunesDeadInstanceAndItsOrphanedSocket(t *testing.T) {
	e := newEnv(t)
	sockPath := filepath.Join(e.sock, "4909.sock")
	entry := e.write(t, "4909", Instance{
		V: 1, PID: deadPID,
		Servername: ptr(sockPath),
		Cwd:        "/tmp", Root: "/tmp", Label: "dead",
	}, true)

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("precondition: socket should exist, got %v", err)
	}

	list, pruned := e.reg.List()
	if len(list) != 0 {
		t.Fatalf("expected no live instances, got %d", len(list))
	}
	if len(pruned) != 1 {
		t.Fatalf("expected 1 pruned entry, got %v", pruned)
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Errorf("registry entry still present: %v", err)
	}
	// The regression: nvim does not clean up after SIGKILL, so the daemon must.
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("orphaned socket was NOT unlinked: %v", err)
	}
}

func TestPruneNeverUnlinksOutsideSockDir(t *testing.T) {
	e := newEnv(t)
	// A hostile or simply mistaken entry pointing at a file we do not own.
	outside := filepath.Join(t.TempDir(), "precious.sock")
	if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.write(t, "666", Instance{
		V: 1, PID: deadPID, Servername: ptr(outside), Cwd: "/tmp",
	}, false)

	e.reg.List()

	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a socket outside SockDir must never be unlinked: %v", err)
	}
}

func TestPruneLeavesAutoServernameAlone(t *testing.T) {
	e := newEnv(t)
	// auto_servername lives under $TMPDIR and belongs to nvim, not to us.
	auto := filepath.Join(t.TempDir(), "nvim.4909.0")
	if err := os.WriteFile(auto, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.write(t, "4909", Instance{
		V: 1, PID: deadPID, Servername: nil, AutoServername: ptr(auto), Cwd: "/tmp",
	}, false)

	e.reg.List()

	if _, err := os.Stat(auto); err != nil {
		t.Errorf("auto_servername must not be unlinked: %v", err)
	}
}

func TestPruneMethodRemovesEntryAndSocket(t *testing.T) {
	e := newEnv(t)
	sockPath := filepath.Join(e.sock, "1234.sock")
	entry := e.write(t, "1234", Instance{
		V: 1, PID: os.Getpid(), Servername: ptr(sockPath), Cwd: "/tmp",
	}, true)

	list, _ := e.reg.List()
	if len(list) != 1 {
		t.Fatalf("expected the live instance to survive, got %d", len(list))
	}
	// Simulates the E247 path: nothing is listening any more.
	e.reg.Prune(list[0])

	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Errorf("entry survived Prune: %v", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket survived Prune: %v", err)
	}
}

func TestListKeepsLiveInstanceAndSocket(t *testing.T) {
	e := newEnv(t)
	sockPath := filepath.Join(e.sock, "self.sock")
	e.write(t, "self", Instance{
		V: 1, PID: os.Getpid(), Servername: ptr(sockPath),
		Cwd: "/tmp", Label: "alive",
	}, true)

	list, pruned := e.reg.List()
	if len(list) != 1 || list[0].Label != "alive" {
		t.Fatalf("live instance was pruned: list=%v pruned=%v", list, pruned)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Errorf("a live instance's socket must not be unlinked: %v", err)
	}
	if list[0].Socket() != sockPath {
		t.Errorf("Socket() = %q, want %q", list[0].Socket(), sockPath)
	}
}

func TestSocketFallsBackToAutoServername(t *testing.T) {
	e := newEnv(t)
	auto := filepath.Join(e.sock, "auto.sock")
	e.write(t, "auto", Instance{
		V: 1, PID: os.Getpid(), Servername: nil, AutoServername: ptr(auto), Cwd: "/tmp",
	}, false)
	if err := os.WriteFile(auto, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	list, _ := e.reg.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(list))
	}
	if got := list[0].Socket(); got != auto {
		t.Errorf("Socket() = %q, want the auto_servername %q", got, auto)
	}
}

func TestListPrunesCorruptAndSocketlessEntries(t *testing.T) {
	e := newEnv(t)
	corrupt := filepath.Join(e.dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Live pid, but neither socket path exists on disk.
	nosock := e.write(t, "nosock", Instance{
		V: 1, PID: os.Getpid(),
		Servername: ptr(filepath.Join(e.sock, "missing.sock")), Cwd: "/tmp",
	}, false)

	list, pruned := e.reg.List()
	if len(list) != 0 {
		t.Fatalf("expected everything pruned, got %d", len(list))
	}
	if len(pruned) != 2 {
		t.Fatalf("expected 2 pruned, got %v", pruned)
	}
	for _, p := range []string{corrupt, nosock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived pruning", filepath.Base(p))
		}
	}
}

func TestIDAndSpawnTolerateNulls(t *testing.T) {
	i := Instance{PID: 4909}
	if i.ID() != "4909" {
		t.Errorf("ID() = %q, want 4909", i.ID())
	}
	// spawn_id is written as JSON null when nvim was not spawned by codelink.
	if i.Spawn() != "" {
		t.Errorf("Spawn() = %q, want empty for a null spawn_id", i.Spawn())
	}
	var decoded Instance
	if err := json.Unmarshal([]byte(`{"pid":1,"spawn_id":null,"servername":null}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Spawn() != "" || decoded.Socket() != "" {
		t.Errorf("null fields must decode to empty, got %q / %q", decoded.Spawn(), decoded.Socket())
	}
}

func TestBySpawnID(t *testing.T) {
	e := newEnv(t)
	for i, spawn := range []string{"aaa", "bbb"} {
		sockPath := filepath.Join(e.sock, fmt.Sprintf("%d.sock", i))
		e.write(t, fmt.Sprintf("inst%d", i), Instance{
			V: 1, PID: os.Getpid(), Servername: ptr(sockPath),
			SpawnID: ptr(spawn), Cwd: "/tmp",
		}, true)
	}
	if got := e.reg.BySpawnID("bbb"); got == nil || got.Spawn() != "bbb" {
		t.Errorf("BySpawnID(bbb) = %v", got)
	}
	if got := e.reg.BySpawnID("zzz"); got != nil {
		t.Errorf("BySpawnID(zzz) = %v, want nil", got)
	}
	if got := e.reg.BySpawnID(""); got != nil {
		t.Errorf("BySpawnID(\"\") must not match a null spawn_id, got %v", got)
	}
}
