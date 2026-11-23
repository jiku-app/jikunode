package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// cmdUninstall removes the per-user service (a systemd unit on Linux, the
// tray app's autostart entry on Windows) and the installed binaries.
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

	removedService, err := removeUserService()
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
	if isBinaryName(filepath.Base(binaryPath)) {
		removedBinary, err = removeInstalledBinary(binaryPath)
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
		afterPurge()
	}

	if !removedService && !removedBinary && !removedData {
		fmt.Fprintln(stdout, "jikunode: nothing to uninstall")
		return 0
	}
	fmt.Fprintln(stdout, "jikunode: user service and binary removed")
	if note := uninstallNote(); note != "" {
		fmt.Fprintln(stdout, note)
	}
	if *purge {
		fmt.Fprintln(stdout, "jikunode: node config, key, state and cache purged")
		if purgeNote != "" {
			fmt.Fprintln(stdout, purgeNote)
		}
	}
	return 0
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
