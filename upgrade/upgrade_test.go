package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestUpgrade(t *testing.T) {
	for _, scenario := range []string{"success", "signature", "checksum", "missing_signature", "smoke", "traversal", "symlink", "extra", "check", "lock", "readonly", "oversized", "metadata_oversized"} {
		t.Run(scenario, func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			binary := []byte("#!/bin/sh\necho 'jikunode v1.2.0'\n")
			if scenario == "smoke" {
				binary = []byte{0x7f, 'E', 'L', 'F'}
			}
			name := "jikunode"
			kind := byte(tar.TypeReg)
			if scenario == "traversal" {
				name = "../jikunode"
			}
			if scenario == "symlink" {
				kind = tar.TypeSymlink
				binary = nil
			}
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Typeflag: kind, Size: int64(len(binary)), Linkname: "/bin/sh"}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(binary); err != nil {
				t.Fatal(err)
			}
			if scenario == "extra" {
				if err := tw.WriteHeader(&tar.Header{Name: "other", Mode: 0644, Size: 0}); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(archive.Bytes())
			checksum := []byte(hex.EncodeToString(hash[:]) + "  jikunode_1.2.0_linux_amd64.tar.gz\n")
			if scenario == "checksum" {
				checksum = []byte(strings.Repeat("0", 64) + "  jikunode_1.2.0_linux_amd64.tar.gz\n")
			}
			if scenario == "metadata_oversized" {
				checksum = bytes.Repeat([]byte("a"), int(MaxChecksumsSize+1))
			}
			sig := ed25519.Sign(priv, append([]byte(SignatureDomain), checksum...))
			if scenario == "signature" {
				sig[0] ^= 1
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch filepath.Base(r.URL.Path) {
				case "checksums.txt":
					_, _ = w.Write(checksum)
				case "checksums.txt.sig":
					if scenario == "missing_signature" {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write(sig)
				default:
					if scenario == "oversized" {
						_, _ = w.Write(bytes.Repeat([]byte("a"), int(MaxArchiveSize+1)))
						return
					}
					_, _ = w.Write(archive.Bytes())
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			exe := filepath.Join(dir, "jikunode")
			if err := os.WriteFile(exe, []byte("old binary"), 0755); err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			if scenario == "lock" {
				f, err := os.OpenFile(filepath.Join(state, "upgrade.lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "readonly" {
				if err := os.Chmod(dir, 0555); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(dir, 0755)
			}
			result, err := Upgrade(context.Background(), Options{Version: "v1.2.0", CurrentVersion: "v1.1.0", Executable: exe, StateDir: state, PublicKey: pub, GOARCH: "amd64", BaseURL: server.URL, Client: server.Client(), Check: scenario == "check"})
			if scenario == "success" || scenario == "check" {
				if err != nil {
					t.Fatal(err)
				}
				if !result.Available {
					t.Fatal("not available")
				}
			} else if err == nil {
				t.Fatal("expected refusal")
			}
			got, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "success" {
				if !bytes.Equal(got, binary) {
					t.Fatal("binary unchanged")
				}
				prev, err := os.ReadFile(filepath.Join(dir, "jikunode.prev"))
				if err != nil || string(prev) != "old binary" {
					t.Fatalf("prev: %q %v", prev, err)
				}
			} else if string(got) != "old binary" {
				t.Fatal("failure changed executable")
			}
			if scenario == "check" {
				entries, err := os.ReadDir(state)
				if err != nil || len(entries) != 0 {
					t.Fatal("check wrote state")
				}
				entries, _ = os.ReadDir(dir)
				if len(entries) != 1 {
					t.Fatal("check wrote executable directory")
				}
			}
		})
	}
}
func TestVersionRefusals(t *testing.T) {
	for _, v := range []string{"v1.0.0", "v0.9.0", "v2.0.0-rc.1", "v2.0", "v02.0.0", "../../evil", "v2.0.0+build"} {
		t.Run(v, func(t *testing.T) {
			if _, err := Upgrade(context.Background(), Options{Version: v, CurrentVersion: "v1.0.0"}); err == nil {
				t.Fatal("accepted invalid upgrade")
			}
		})
	}
}
func TestMissingKey(t *testing.T) {
	_, err := Upgrade(context.Background(), Options{Version: "v2.0.0", CurrentVersion: "v1.0.0", PublicKey: make([]byte, 32)})
	if err == nil || !strings.Contains(err.Error(), "release key not provisioned") {
		t.Fatal(err)
	}
}
func TestLatestCheck(t *testing.T) {
	var base string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest" {
			t.Error("unexpected download")
		}
		http.Redirect(w, r, base+"/releases/tag/v2.0.0", http.StatusFound)
	}))
	defer s.Close()
	base = s.URL
	result, err := Upgrade(context.Background(), Options{CurrentVersion: "v1.0.0", Check: true, BaseURL: base, Client: s.Client()})
	if err != nil || !result.Available {
		t.Fatalf("%+v %v", result, err)
	}
}
func TestRedirectHTTPSOnly(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/evil", http.StatusFound)
	}))
	defer s.Close()
	_, err := Upgrade(context.Background(), Options{CurrentVersion: "v1.0.0", Check: true, BaseURL: s.URL, Client: s.Client()})
	if err == nil {
		t.Fatal(fmt.Errorf("accepted plaintext redirect"))
	}
}
