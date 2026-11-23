//go:build windows

package winacl

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trustedSIDs are principals that may hold access to a private file besides
// the current user. LocalSystem and Administrators can take ownership of any
// file anyway, the way root can read a 0600 file on Unix.
var trustedSIDs = []windows.WELL_KNOWN_SID_TYPE{
	windows.WinLocalSystemSid,
	windows.WinBuiltinAdministratorsSid,
}

func currentUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current user: %w", err)
	}
	return user.User.Sid.Copy()
}

// RestrictToOwner replaces the DACL of path with a protected one (no inherited
// entries) that grants full control to the current user and LocalSystem only.
// For a directory the entries are inherited by everything created inside it.
func RestrictToOwner(path string, dir bool) error {
	user, err := currentUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if dir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, 2)
	for _, sid := range []*windows.SID{user, system} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build access list: %w", err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("set access list: %w", err)
	}
	return nil
}

const (
	accessAllowedACEType = 0x0
	inheritOnlyACE       = 0x8
)

// CheckOwnerOnly returns an error unless every allow entry in the DACL of path
// grants access to the current user, LocalSystem or Administrators only. A
// missing (NULL) DACL grants everyone access and is refused.
func CheckOwnerOnly(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read access list: %w", err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return errors.New("file has no access list (everyone may read it)")
		}
		return fmt.Errorf("read access list: %w", err)
	}
	if acl == nil {
		return errors.New("file has no access list (everyone may read it)")
	}
	user, err := currentUser()
	if err != nil {
		return err
	}
	allowed := []*windows.SID{user}
	for _, kind := range trustedSIDs {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return err
		}
		allowed = append(allowed, sid)
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return fmt.Errorf("read access entry: %w", err)
		}
		if ace.Header.AceType != accessAllowedACEType || ace.Header.AceFlags&inheritOnlyACE != 0 || ace.Mask == 0 {
			continue // deny entries and inherit-only entries grant nothing here
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := false
		for _, a := range allowed {
			if sid.Equals(a) {
				trusted = true
				break
			}
		}
		if !trusted {
			return fmt.Errorf("file is accessible by %s", sid.String())
		}
	}
	return nil
}
