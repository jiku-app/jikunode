//go:build !windows

package nodeagent

import (
	"fmt"
	"os"
)

// securePrivateFile restricts an open file to its owner (mode 0600).
func securePrivateFile(f *os.File) error { return f.Chmod(fileMode) }

// securePrivateDir restricts a directory to its owner (mode 0700).
func securePrivateDir(dir string) error { return os.Chmod(dir, dirMode) }

// transientFileError reports whether err may clear up on its own if the
// operation is retried. Unix has no such errors here.
func transientFileError(error) bool { return false }

// checkPrivateFile refuses a file readable by group or others (fail closed).
func checkPrivateFile(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private key %s is accessible by group or others (mode %04o); refusing",
			path, info.Mode().Perm())
	}
	return nil
}
