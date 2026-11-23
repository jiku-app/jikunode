//go:build windows

package nodeagent

import (
	"fmt"
	"os"

	"github.com/jiku-app/jikunode/internal/winacl"
)

// securePrivateFile gives an open file a protected DACL granting access only
// to the current user and LocalSystem: the Windows form of mode 0600.
func securePrivateFile(f *os.File) error { return winacl.RestrictToOwner(f.Name(), false) }

// securePrivateDir gives a directory the same owner-only, inheritable DACL:
// the Windows form of mode 0700.
func securePrivateDir(dir string) error { return winacl.RestrictToOwner(dir, true) }

// checkPrivateFile refuses a key file that any principal other than the
// current user, LocalSystem or the Administrators group may access (fail
// closed). Windows reports every file as mode 0666, so mode bits say nothing.
func checkPrivateFile(path string, _ os.FileInfo) error {
	if err := winacl.CheckOwnerOnly(path); err != nil {
		return fmt.Errorf("private key %s: %w; refusing", path, err)
	}
	return nil
}
