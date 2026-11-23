//go:build windows

package upgrade

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildFake compiles testdata/fakebin linked with version.
func buildFake(t *testing.T, version string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fake.exe")
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", "-X main.Version="+version, "./testdata/fakebin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake binary: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type zipEntry struct {
	name string
	data []byte
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveRelease serves a signed v1.2.0 release holding archive for windows/amd64.
func serveRelease(t *testing.T, archive []byte) (*httptest.Server, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(archive)
	checksum := []byte(hex.EncodeToString(hash[:]) + "  " + AssetName("v1.2.0", "windows", "amd64") + "\n")
	sig := ed25519.Sign(priv, append([]byte(SignatureDomain), checksum...))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "checksums.txt":
			_, _ = w.Write(checksum)
		case "checksums.txt.sig":
			_, _ = w.Write(sig)
		case AssetName("v1.2.0", "windows", "amd64"):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, pub
}

func TestUpgradeWindows(t *testing.T) {
	newBin := buildFake(t, "v1.2.0")
	oldBin := buildFake(t, "v1.1.0")
	wrongBin := buildFake(t, "v9.9.9")

	cases := []struct {
		name    string
		archive func() []byte
		running bool // the old node and tray are running during the upgrade
		prevRun bool // an older jikunode-tray.prev.exe is still running too
		wantErr string
	}{
		{name: "success", archive: func() []byte {
			return zipOf(t, zipEntry{WindowsBinary, newBin}, zipEntry{WindowsTrayBinary, newBin})
		}},
		{name: "running", running: true, archive: func() []byte {
			return zipOf(t, zipEntry{WindowsBinary, newBin}, zipEntry{WindowsTrayBinary, newBin})
		}},
		{name: "prev_running", running: true, prevRun: true, archive: func() []byte {
			return zipOf(t, zipEntry{WindowsBinary, newBin}, zipEntry{WindowsTrayBinary, newBin})
		}},
		{name: "node_only", archive: func() []byte { return zipOf(t, zipEntry{WindowsBinary, newBin}) }},
		{name: "missing_main", wantErr: "missing", archive: func() []byte {
			return zipOf(t, zipEntry{WindowsTrayBinary, newBin})
		}},
		{name: "extra", wantErr: "invalid archive entry", archive: func() []byte {
			return zipOf(t, zipEntry{WindowsBinary, newBin}, zipEntry{"evil.dll", []byte("x")})
		}},
		{name: "traversal", wantErr: "invalid archive entry", archive: func() []byte {
			return zipOf(t, zipEntry{"../" + WindowsBinary, newBin})
		}},
		{name: "duplicate", wantErr: "duplicate", archive: func() []byte {
			return zipOf(t, zipEntry{WindowsBinary, newBin}, zipEntry{WindowsBinary, newBin})
		}},
		{name: "smoke", wantErr: "version check", archive: func() []byte {
			return zipOf(t, zipEntry{WindowsBinary, wrongBin})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, pub := serveRelease(t, tc.archive())
			dir := t.TempDir()
			exe := filepath.Join(dir, WindowsBinary)
			tray := filepath.Join(dir, WindowsTrayBinary)
			for _, p := range []string{exe, tray} {
				if err := os.WriteFile(p, oldBin, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.prevRun {
				prev := PrevPath(tray)
				if err := os.WriteFile(prev, oldBin, 0755); err != nil {
					t.Fatal(err)
				}
				startIdle(t, prev)
			}
			if tc.running {
				startIdle(t, exe)
				startIdle(t, tray)
			}
			_, err := Upgrade(context.Background(), Options{
				Version: "v1.2.0", CurrentVersion: "v1.1.0", Executable: exe, StateDir: t.TempDir(),
				PublicKey: pub, GOOS: "windows", GOARCH: "amd64", BaseURL: server.URL, Client: server.Client(),
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if got, _ := os.ReadFile(exe); !bytes.Equal(got, oldBin) {
					t.Fatal("failed upgrade changed the executable")
				}
				assertNoStaged(t, dir)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(exe); !bytes.Equal(got, newBin) {
				t.Fatal("executable not replaced")
			}
			if got, _ := os.ReadFile(PrevPath(exe)); !bytes.Equal(got, oldBin) {
				t.Fatal("previous executable not kept")
			}
			wantTray := oldBin
			if tc.name != "node_only" {
				wantTray = newBin
			}
			if got, _ := os.ReadFile(tray); !bytes.Equal(got, wantTray) {
				t.Fatal("tray binary in wrong state")
			}
			assertNoStaged(t, dir)
		})
	}
}

func TestUpgradeLockWindows(t *testing.T) {
	state := t.TempDir()
	f, err := os.OpenFile(filepath.Join(state, "upgrade.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tryLock(f); err != nil {
		t.Fatal(err)
	}
	server, pub := serveRelease(t, zipOf(t))
	exe := filepath.Join(t.TempDir(), WindowsBinary)
	if err := os.WriteFile(exe, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err = Upgrade(context.Background(), Options{
		Version: "v1.2.0", CurrentVersion: "v1.1.0", Executable: exe, StateDir: state,
		PublicKey: pub, GOOS: "windows", GOARCH: "amd64", BaseURL: server.URL, Client: server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("err = %v", err)
	}
}

func startIdle(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// assertNoStaged checks that no staged upgrade file is left behind. Old
// binaries moved aside while running (.jikunode-old-*) are allowed.
func assertNoStaged(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".jikunode-upgrade-") {
			t.Fatalf("staged file left behind: %s", e.Name())
		}
	}
}
