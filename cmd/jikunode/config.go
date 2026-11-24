// Command jikunode is the supplier-side egress agent: it binds itself to a
// supplier with a reusable binding code, keeps an outbound websocket tunnel to
// the hub and dials upstream hosts on the hub's behalf.
//
// It holds no credentials of any kind. The only secret it stores is its own
// ed25519 private key (mode 0600), and the loopback status page never shows or
// accepts it.
package main

import (
	"crypto/ed25519"
	"path/filepath"

	"github.com/jiku-app/jikunode/nodeagent"
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

// configDir returns $XDG_CONFIG_HOME/jikunode, falling back to
// ~/.config/jikunode on Unix and %APPDATA%\jikunode on Windows.
func configDir() (string, error) {
	root, err := configRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, configDirName), nil
}

// configPath returns the config file path.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

func loadConfig() (*nodeagent.Config, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	return nodeagent.LoadConfig(dir)
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) { return nodeagent.LoadPrivateKey(path) }
