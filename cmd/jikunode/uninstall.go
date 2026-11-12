package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const userServiceName = "jikunode.service"

// cmdUninstall removes the per-user systemd unit and the running binary.
// --purge additionally removes the node identity and local state/cache. It
// deliberately does not delete the user's shared systemd journal.
func cmdUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "also remove the node key, config, state and cache")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "uninstall takes no positional arguments")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopUserService(ctx)

	servicePath, err := userServicePath()
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	removedService, err := removeIfExists(servicePath)
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: remove user service: %v\n", err)
		return 1
	}

	binaryPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: locate executable: %v\n", err)
		return 1
	}
	// Refuse to remove an arbitrary executable if invoked through an unusual
	// path (for example `go run` or a renamed program).
	removedBinary := false
	if filepath.Base(binaryPath) == "jikunode" {
		removedBinary, err = removeIfExists(binaryPath)
		if err != nil {
			fmt.Fprintf(stderr, "jikunode: remove binary: %v\n", err)
			return 1
		}
	}

	removedData := false
	if *purge {
		paths, err := purgePaths()
		if err != nil {
			fmt.Fprintf(stderr, "jikunode: locate data: %v\n", err)
			return 1
		}
		for _, path := range paths {
			removed, err := removePrivateTree(path)
			if err != nil {
				fmt.Fprintf(stderr, "jikunode: purge %s: %v\n", path, err)
				return 1
			}
			removedData = removedData || removed
		}
	}

	if !removedService && !removedBinary && !removedData {
		fmt.Fprintln(stdout, "jikunode: nothing to uninstall")
		return 0
	}
	fmt.Fprintln(stdout, "jikunode: user service and binary removed")
	if *purge {
		fmt.Fprintln(stdout, "jikunode: node config, key, state and cache purged")
		fmt.Fprintln(stdout, "jikunode: systemd journal history was not removed")
	}
	return 0
}

func userServicePath() (string, error) {
	root, err := configRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "systemd", "user", userServiceName), nil
}

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

func stopUserService(ctx context.Context) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	// The unit may not exist or may already be stopped; uninstall is
	// intentionally idempotent, so these errors are ignored.
	_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", userServiceName).Run()
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
}

func removeIfExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

func removePrivateTree(path string) (bool, error) {
	if filepath.Base(path) != configDirName {
		return false, fmt.Errorf("refusing to remove unexpected path")
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	return true, nil
}
