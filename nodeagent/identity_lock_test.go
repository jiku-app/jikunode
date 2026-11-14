package nodeagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A crashed creator leaves its lock directory behind. Once it is stale the next
// creator takes it over instead of waiting forever.
func TestCreateExclusiveTakesOverStaleLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.id")
	lock := path + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	created, err := createExclusive(path, []byte("x\n"))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
}

// Creation leaves exactly the final file: no lock directory, no temp file. The
// identity directory is never touched with hard links (Android forbids them).
func TestCreateExclusiveLeavesOnlyTheFinalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	created, err := createExclusive(path, []byte("secret\n"))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "node.key" {
		t.Fatalf("unexpected directory content: %v", entries)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != fileMode {
		t.Fatalf("mode %v err %v", info.Mode().Perm(), err)
	}
	// A second create reports the file already existed and keeps its content.
	created, err = createExclusive(path, []byte("other\n"))
	if err != nil || created {
		t.Fatalf("second create: created=%v err=%v", created, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "secret\n" {
		t.Fatalf("content overwritten: %q", got)
	}
}
