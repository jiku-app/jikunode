package upgrade

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupStale removes leftovers of earlier upgrades in the directory of
// executable: staged files of an interrupted upgrade and, on Windows, old
// binaries that were still running when they were replaced. Files that are
// still in use are left for a later call.
func CleanupStale(executable string) {
	dir := filepath.Dir(executable)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		stale := strings.HasPrefix(name, ".jikunode-old-")
		if strings.HasPrefix(name, ".jikunode-upgrade-") {
			// A staged file may belong to an upgrade running right now
			// (it holds the lock for at most five minutes).
			info, err := e.Info()
			stale = err == nil && time.Since(info.ModTime()) > 10*time.Minute
		}
		if stale {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
