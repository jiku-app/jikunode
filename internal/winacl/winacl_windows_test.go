//go:build windows

package winacl

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func grantEveryone(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictAndCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RestrictToOwner(dir, true); err != nil {
		t.Fatal(err)
	}
	// A file created later inherits the directory's owner-only entries.
	inherited := filepath.Join(dir, "inherited")
	if err := os.WriteFile(inherited, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnerOnly(inherited); err != nil {
		t.Fatalf("inherited file: %v", err)
	}

	key := filepath.Join(dir, "node.key")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestrictToOwner(key, false); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnerOnly(key); err != nil {
		t.Fatalf("restricted file: %v", err)
	}
	grantEveryone(t, key)
	if err := CheckOwnerOnly(key); err == nil {
		t.Fatal("file readable by Everyone was accepted")
	}
	// Restricting again repairs it.
	if err := RestrictToOwner(key, false); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnerOnly(key); err != nil {
		t.Fatalf("re-restricted file: %v", err)
	}
}

func TestNullDACLRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "open")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnerOnly(path); err == nil {
		t.Fatal("NULL DACL was accepted")
	}
}
