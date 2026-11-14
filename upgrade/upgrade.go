// Package upgrade installs authenticated JikuNode releases.
package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const Repository = "https://github.com/jiku-app/jikunode"
const SignatureDomain = "jikunode-release-v1:"

// ReleasePublicKey is the base64 raw ed25519 release verification key.
const ReleasePublicKey = "YDmAsBI8V4rSx6/5GTYbhO8EkhaPNlNU6l7C+/Yb0eo="
const MaxArchiveSize int64 = 64 << 20
const MaxChecksumsSize int64 = 64 << 10

var ErrUnsupported = errors.New("upgrade: binary directory is not writable")
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func ValidVersion(v string) bool { return len(v) <= 64 && versionPattern.MatchString(v) }
func Newer(target, current string) bool {
	if !ValidVersion(target) || !ValidVersion(current) {
		return false
	}
	a, b := strings.Split(target[1:], "."), strings.Split(current[1:], ".")
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i])
		}
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

type Options struct {
	Version        string
	CurrentVersion string
	StateDir       string
	Check          bool
	// Test injection points; production callers leave these unset.
	Client     *http.Client
	BaseURL    string
	PublicKey  ed25519.PublicKey
	Executable string
	GOARCH     string
}
type Result struct {
	Version   string
	Available bool
}

// Supported checks the actual directory permission, including read-only mounts.
func Supported(executable string) error {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	dir := filepath.Dir(executable)
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0222 == 0 {
		return ErrUnsupported
	}
	f, err := os.CreateTemp(dir, ".jikunode-probe-*")
	if err != nil {
		return ErrUnsupported
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
func bounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("upgrade: size limit exceeded")
	}
	return b, nil
}

func Upgrade(ctx context.Context, opts Options) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	base := opts.BaseURL
	if base == "" {
		base = Repository
	}
	client := http.Client{}
	if opts.Client != nil {
		client = *opts.Client
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("upgrade: non-https redirect refused")
		}
		if len(via) >= 10 {
			return errors.New("upgrade: too many redirects")
		}
		return nil
	}
	get := func(path string, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("upgrade: download failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New("upgrade: release asset unavailable")
		}
		return bounded(resp.Body, limit)
	}
	target := opts.Version
	if target == "" {
		latest := client
		latest.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("upgrade: non-https redirect refused")
			}
			return http.ErrUseLastResponse
		}
		req, err := http.NewRequestWithContext(ctx, "GET", base+"/releases/latest", nil)
		if err != nil {
			return Result{}, err
		}
		resp, err := latest.Do(req)
		if err != nil {
			return Result{}, errors.New("upgrade: latest lookup failed")
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return Result{}, errors.New("upgrade: missing latest redirect")
		}
		loc, err := resp.Location()
		if err != nil || loc.Scheme != "https" || loc.Host != req.URL.Host || !strings.HasPrefix(loc.Path, req.URL.Path[:len(req.URL.Path)-len("latest")]+"tag/") {
			return Result{}, errors.New("upgrade: invalid latest redirect")
		}
		target = filepath.Base(loc.Path)
	}
	result := Result{Version: target, Available: Newer(target, opts.CurrentVersion)}
	if !ValidVersion(target) || !ValidVersion(opts.CurrentVersion) {
		return result, errors.New("upgrade: invalid stable version")
	}
	if !result.Available {
		if opts.Check {
			return result, nil
		}
		return result, errors.New("upgrade: target must be strictly newer")
	}
	if opts.Check {
		return result, nil
	}
	key := opts.PublicKey
	if key == nil {
		var err error
		key, err = base64.StdEncoding.DecodeString(ReleasePublicKey)
		if err != nil {
			return result, errors.New("upgrade: release key not provisioned")
		}
	}
	if len(key) != ed25519.PublicKeySize || bytes.Equal(key, make([]byte, ed25519.PublicKeySize)) {
		return result, errors.New("upgrade: release key not provisioned")
	}
	executable := opts.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return result, err
		}
	}
	if err := Supported(executable); err != nil {
		return result, err
	}
	if opts.StateDir == "" {
		return result, errors.New("upgrade: state directory required")
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(filepath.Join(opts.StateDir, "upgrade.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, errors.New("upgrade: already in progress")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	arch := opts.GOARCH
	if arch == "" {
		arch = runtime.GOARCH
	}
	asset := fmt.Sprintf("jikunode_%s_linux_%s.tar.gz", target[1:], arch)
	prefix := "/releases/download/" + target + "/"
	checksums, err := get(prefix+"checksums.txt", MaxChecksumsSize)
	if err != nil {
		return result, err
	}
	sig, err := get(prefix+"checksums.txt.sig", 1024)
	if err != nil {
		return result, err
	}
	if len(sig) != ed25519.SignatureSize {
		sig, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil {
			return result, errors.New("upgrade: invalid release signature")
		}
	}
	if !ed25519.Verify(key, append([]byte(SignatureDomain), checksums...), sig) {
		return result, errors.New("upgrade: invalid release signature")
	}
	expected := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			if expected != "" {
				return result, errors.New("upgrade: duplicate checksum")
			}
			expected = fields[0]
		}
	}
	archive, err := get(prefix+asset, MaxArchiveSize)
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(archive)
	if expected != hex.EncodeToString(sum[:]) {
		return result, errors.New("upgrade: checksum mismatch")
	}
	f, err := os.CreateTemp(filepath.Dir(executable), ".jikunode-upgrade-*")
	if err != nil {
		return result, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return result, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		return result, err
	}
	if h.Name != "jikunode" || h.Typeflag != tar.TypeReg || h.Size > MaxArchiveSize {
		return result, errors.New("upgrade: invalid archive entry")
	}
	if _, err = io.Copy(f, io.LimitReader(tr, MaxArchiveSize+1)); err != nil {
		return result, err
	}
	if _, err = tr.Next(); err != io.EOF {
		return result, errors.New("upgrade: archive must contain exactly one binary")
	}
	if err = f.Chmod(0755); err != nil {
		return result, err
	}
	if err = f.Sync(); err != nil {
		return result, err
	}
	if err = f.Close(); err != nil {
		return result, err
	}
	cmd := exec.CommandContext(ctx, f.Name(), "version")
	var output limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil || strings.TrimSpace(output.String()) != "jikunode "+target {
		return result, errors.New("upgrade: binary version check failed")
	}
	prev := filepath.Join(filepath.Dir(executable), "jikunode.prev")
	if err = os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	// A hard link preserves the old inode while the live path remains intact until rename.
	if err = os.Link(executable, prev); err != nil {
		return result, err
	}
	if err = os.Rename(f.Name(), executable); err != nil {
		return result, err
	}
	return result, nil
}

type limitedOutput struct{ bytes.Buffer }

func (w *limitedOutput) Write(b []byte) (int, error) {
	if w.Len()+len(b) > 128 {
		return 0, errors.New("upgrade: excessive version output")
	}
	return w.Buffer.Write(b)
}
