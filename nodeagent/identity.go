package nodeagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// idFileName holds the node UUID created together with the key, before
	// any binding exists.
	idFileName = "node.id"
	// boundFileName is the bound marker. It is written last, after config.json.
	boundFileName = "bound"
)

// Identity is the offline node identity: a UUID and the path of its ed25519
// key. It exists before the node is bound and never involves the network.
type Identity struct {
	NodeID  string
	KeyPath string
}

// EnsureIdentity returns the identity in dir, creating the UUID and the
// ed25519 key locally when they are missing. It makes no network call. Both
// files are installed with an exclusive atomic create (lock directory + temp file + rename), so
// two processes (the idle service and the CLI) can never overwrite each other:
// the loser reloads what the winner wrote.
//
// A directory that already holds a binding (the bound marker or config.json)
// must have an intact node.key and node.id: a missing or corrupt file is a
// damaged identity and fails with an error. A replacement is never generated,
// because it would silently turn the node into a different, unbound identity.
func EnsureIdentity(dir string) (*Identity, ed25519.PrivateKey, error) {
	if err := ensureConfigDir(dir); err != nil {
		return nil, nil, err
	}
	if hasBinding(dir) {
		return loadBoundIdentity(dir)
	}
	keyPath := filepath.Join(dir, keyFileName)
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, nil, err
	}
	id, err := loadOrCreateNodeID(filepath.Join(dir, idFileName))
	if err != nil {
		return nil, nil, err
	}
	return &Identity{NodeID: id, KeyPath: keyPath}, key, nil
}

// hasBinding reports whether dir holds any trace of a binding: the marker or
// the config written before it.
func hasBinding(dir string) bool {
	for _, name := range []string{boundFileName, configFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func damaged(dir, file, problem string) error {
	return fmt.Errorf("bound identity is damaged: %s %s; remove %s to start over", file, problem, dir)
}

// loadBoundIdentity loads the identity of a bound directory without ever
// creating a file.
func loadBoundIdentity(dir string) (*Identity, ed25519.PrivateKey, error) {
	keyPath := filepath.Join(dir, keyFileName)
	key, err := LoadPrivateKey(keyPath)
	if err != nil {
		return nil, nil, damaged(dir, keyFileName, problemOf(err))
	}
	id, err := readNodeID(filepath.Join(dir, idFileName))
	if err != nil {
		return nil, nil, damaged(dir, idFileName, problemOf(err))
	}
	if _, err := os.Stat(filepath.Join(dir, configFileName)); err == nil {
		cfg, err := LoadConfig(dir)
		if err != nil {
			return nil, nil, damaged(dir, configFileName, "invalid")
		}
		if cfg.NodeID != id {
			return nil, nil, damaged(dir, configFileName, "does not match "+idFileName)
		}
	} else {
		return nil, nil, damaged(dir, configFileName, "missing")
	}
	return &Identity{NodeID: id, KeyPath: keyPath}, key, nil
}

func problemOf(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	return "invalid"
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	key, err := LoadPrivateKey(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	_, fresh, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	created, err := createExclusive(path, keyFileBytes(fresh))
	if err != nil {
		return nil, err
	}
	if created {
		return fresh, nil
	}
	return LoadPrivateKey(path)
}

func loadOrCreateNodeID(path string) (string, error) {
	if id, err := readNodeID(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return id, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	h := hex.EncodeToString(raw[:])
	fresh := fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
	created, err := createExclusive(path, []byte(fresh+"\n"))
	if err != nil {
		return "", err
	}
	if created {
		return fresh, nil
	}
	return readNodeID(path)
}

func readNodeID(path string) (string, error) {
	data, err := retryTransient(func() ([]byte, error) { return os.ReadFile(path) })
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !validNodeID(id) {
		return "", fmt.Errorf("%s does not hold a valid node id", path)
	}
	return id, nil
}

// lockStale is how old an identity lock directory must be before a crashed
// holder is assumed and the lock is taken over.
const lockStale = 30 * time.Second

// lockTimeout bounds how long a process waits for another creator.
const lockTimeout = 10 * time.Second

// transientWait bounds how long a file operation is retried while it fails
// with a transientFileError.
const transientWait = 2 * time.Second

// retryTransient runs op until it succeeds, fails for good, or keeps failing
// with a transient error for transientWait.
func retryTransient[T any](op func() (T, error)) (T, error) {
	deadline := time.Now().Add(transientWait)
	for {
		v, err := op()
		if err == nil || !transientFileError(err) || time.Now().After(deadline) {
			return v, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// createExclusive installs data at path only if path does not exist yet and
// reports whether this call created the file. Creation is serialised with a
// lock directory (mkdir is atomic everywhere, including Android app storage,
// where hard links are forbidden) and the content is complete before it
// becomes visible (synced temp file, then rename), so a reader never sees a
// partial file and two creators can never overwrite each other.
func createExclusive(path string, data []byte) (bool, error) {
	lock := path + ".lock"
	deadline := time.Now().Add(lockTimeout)
	for {
		if _, err := os.Lstat(path); err == nil {
			return false, nil
		}
		err := os.Mkdir(lock, dirMode)
		if err == nil {
			break
		}
		// On Windows a lock directory that another creator is still
		// removing cannot be created again for a moment: wait as for a held
		// lock.
		if !errors.Is(err, os.ErrExist) && !transientFileError(err) {
			return false, fmt.Errorf("lock identity file: %w", err)
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > lockStale {
			_ = os.Remove(lock) // crashed holder
			continue
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("timed out waiting for another process creating %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer os.Remove(lock)
	// Another creator may have finished between our check and our lock.
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".jikunode-*")
	if err != nil {
		return false, fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := securePrivateFile(f); err != nil {
		f.Close()
		return false, fmt.Errorf("set file mode: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, fmt.Errorf("write file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, fmt.Errorf("sync file: %w", err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("close file: %w", err)
	}
	if _, err := retryTransient(func() (struct{}, error) { return struct{}{}, os.Rename(tmp, path) }); err != nil {
		return false, fmt.Errorf("install file: %w", err)
	}
	return true, nil
}

// IsBound is the single definition of "bound": the bound marker exists, the
// config is readable and valid, and the private key it names is readable.
func IsBound(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, boundFileName)); err != nil {
		return false
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return false
	}
	_, err = LoadPrivateKey(cfg.KeyPath)
	return err == nil
}

func validNodeID(id string) bool {
	if id != strings.ToLower(id) || len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	b, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(b) == 16 && b[6]>>4 == 4 && b[8]>>6 == 2
}
