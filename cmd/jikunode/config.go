// Command jikunode is the supplier-side egress agent: it binds itself to a
// supplier with a one-time binding code, keeps an outbound websocket tunnel to
// the hub and dials upstream hosts on the hub's behalf.
//
// It holds no credentials of any kind. The only secret it stores is its own
// ed25519 private key (mode 0600), and the loopback status page never shows or
// accepts it.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// configDirName is the directory under the XDG config root.
	configDirName = "jikunode"
	// configFileName is the JSON config file name.
	configFileName = "config.json"
	// keyFileName is the default private-key file name (raw 32-byte seed,
	// base64) inside the config directory.
	keyFileName = "node.key"

	// dirMode is the config directory mode: owner only.
	dirMode = 0o700
	// fileMode is the config and key file mode: owner read/write only.
	fileMode = 0o600
)

// Config is the whole on-disk node configuration. It deliberately has no
// field for a credential, token or proxy URL.
type Config struct {
	HubURL  string `json:"hub_url"`
	NodeID  string `json:"node_id"`
	KeyPath string `json:"key_path"`
}

// configDir returns $XDG_CONFIG_HOME/jikunode, falling back to
// ~/.config/jikunode.
func configDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, configDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", configDirName), nil
}

// configPath returns the config file path.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// loadConfig reads and validates the config file.
func loadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("not bound yet (no %s); run \"jikunode bind <code> --hub <url>\"", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(cfg.HubURL) == "" {
		return nil, fmt.Errorf("%s has no hub_url", path)
	}
	if strings.TrimSpace(cfg.NodeID) == "" {
		return nil, fmt.Errorf("%s has no node_id", path)
	}
	if strings.TrimSpace(cfg.KeyPath) == "" {
		return nil, fmt.Errorf("%s has no key_path", path)
	}
	return &cfg, nil
}

// saveConfig writes the config atomically with mode 0600, creating the config
// directory with mode 0700.
func saveConfig(cfg *Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := ensureConfigDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(dir, configFileName), data)
}

// ensureConfigDir creates dir with mode 0700 and tightens an existing dir.
func ensureConfigDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("set config directory mode: %w", err)
	}
	return nil
}

// savePrivateKey stores the ed25519 private key as the base64 of its 32-byte
// seed, mode 0600, written atomically.
func savePrivateKey(path string, key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid ed25519 private key")
	}
	seed := key.Seed()
	return writeFileAtomic(path, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"))
}

// loadPrivateKey reads the raw seed and reconstructs the ed25519 private key.
// A key file readable by group or others is refused (fail closed).
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("private key %s is accessible by group or others (mode %04o); refusing",
			path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("private key %s is not a base64 ed25519 seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// writeFileAtomic writes data to a temp file in the target directory with mode
// 0600, fsyncs it and renames it over path. A partially written file is never
// visible under the final name.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".jikunode-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(fileMode); err != nil {
		cleanup()
		return fmt.Errorf("set file mode: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write file: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install file: %w", err)
	}
	return nil
}
