//go:build !windows

// Command jikunode-tray is the Windows tray app for jikunode. On other
// systems use the systemd unit or container described in deploy/jikunode.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "jikunode-tray only runs on Windows; see deploy/jikunode for Linux")
	os.Exit(1)
}
