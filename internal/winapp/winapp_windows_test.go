//go:build windows

package winapp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPortableRoots(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(exe), PortableMarker)
	if _, err := os.Stat(marker); err == nil {
		t.Skip("a portable marker already sits next to the test binary")
	}
	if _, ok := PortableDataDir(); ok {
		t.Fatal("portable without a marker")
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })

	data, ok := PortableDataDir()
	if !ok || data != filepath.Join(filepath.Dir(exe), "data") {
		t.Fatalf("PortableDataDir = %q, %v", data, ok)
	}
	if root, _ := ConfigRoot(); root != filepath.Join(data, "config") {
		t.Errorf("ConfigRoot = %q", root)
	}
	if root, _ := StateRoot(); root != filepath.Join(data, "state") {
		t.Errorf("StateRoot = %q", root)
	}
	// An explicit XDG override still wins.
	t.Setenv("XDG_CONFIG_HOME", `C:\override`)
	if root, _ := ConfigRoot(); root != `C:\override` {
		t.Errorf("ConfigRoot with XDG_CONFIG_HOME = %q", root)
	}
}
