//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// binaryName is the installed executable name uninstall may remove.
	binaryName = "jikunode"
	// userServiceName is the per-user systemd unit.
	userServiceName = "jikunode.service"
	// purgeNote is printed after --purge.
	purgeNote = "jikunode: systemd journal history was not removed"
)

// configRoot is $XDG_CONFIG_HOME or ~/.config.
func configRoot() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// stateRoot is $XDG_STATE_HOME or ~/.local/state.
func stateRoot() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

func purgePaths() ([]string, error) {
	config, err := configDir()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	return []string{
		config,
		filepath.Join(home, ".local", "state", configDirName),
		filepath.Join(home, ".cache", configDirName),
	}, nil
}

func userServicePath() (string, error) {
	root, err := configRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "systemd", "user", userServiceName), nil
}

func stopUserService(ctx context.Context) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	// The unit may not exist or may already be stopped; uninstall is
	// intentionally idempotent, so these errors are ignored.
	_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", userServiceName).Run()
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
}

// removeUserService deletes the systemd unit file.
func removeUserService() (bool, error) {
	path, err := userServicePath()
	if err != nil {
		return false, err
	}
	return removeIfExists(path)
}

// removeInstalledBinary deletes the running binary; Unix allows unlinking a
// running executable.
func removeInstalledBinary(path string) (bool, error) { return removeIfExists(path) }

var restartUpgradedService = func(ctx context.Context) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", userServiceName).Run(); err != nil {
		return nil
	}
	return exec.CommandContext(ctx, "systemctl", "--user", "restart", userServiceName).Run()
}

// isBinaryName reports whether name is the installed executable name.
func isBinaryName(name string) bool { return name == binaryName }
