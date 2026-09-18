//go:build windows

package ask

import (
	"fmt"
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fileAddFile      windows.ACCESS_MASK = 0x0002
	fileDeleteChild  windows.ACCESS_MASK = 0x0040
	objectInheritACE                     = 0x01
)

const directoryWriteMask = fileAddFile |
	fileDeleteChild |
	windows.DELETE |
	windows.WRITE_DAC |
	windows.WRITE_OWNER |
	windows.GENERIC_WRITE |
	windows.GENERIC_ALL

const inheritedSecretAccessMask = windows.FILE_READ_DATA |
	windows.FILE_WRITE_DATA |
	windows.FILE_APPEND_DATA |
	windows.DELETE |
	windows.WRITE_DAC |
	windows.WRITE_OWNER |
	windows.GENERIC_READ |
	windows.GENERIC_WRITE |
	windows.GENERIC_ALL

// checkParentDirAccess enforces the Windows equivalent of the Unix
// group/world-write check. Go reports synthetic 0777 mode bits for Windows
// directories; the DACL is the source of truth.
func checkParentDirAccess(path string, _ fs.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("ask: reading access controls for %s: %w", path, err)
	}
	if sd == nil {
		return fmt.Errorf("ask: refusing to write a secret into %s — it has no security descriptor", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("ask: reading access controls for %s: %w", path, err)
	}
	if dacl == nil {
		return fmt.Errorf("ask: refusing to write a secret into %s — it has no restrictive DACL", path)
	}

	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("ask: reading current Windows identity: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("ask: reading owner for %s: %w", path, err)
	}
	if owner == nil || !trustedWindowsDirectoryOwner(owner, current.User.Sid) {
		return fmt.Errorf("ask: refusing to write a secret into %s — it is owned by another Windows principal", path)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("ask: reading access controls for %s: %w", path, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if trustedWindowsSecretPrincipal(sid, current.User.Sid) {
			continue
		}

		appliesToDirectory := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 &&
			ace.Mask&directoryWriteMask != 0
		inheritsToSecret := ace.Header.AceFlags&objectInheritACE != 0 &&
			ace.Mask&inheritedSecretAccessMask != 0
		if appliesToDirectory || inheritsToSecret {
			return fmt.Errorf(
				"ask: refusing to write a secret into %s — Windows ACL grants another principal unsafe access",
				path,
			)
		}
	}
	return nil
}

func trustedWindowsDirectoryOwner(sid, current *windows.SID) bool {
	return sid.Equals(current) ||
		sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

func trustedWindowsSecretPrincipal(sid, current *windows.SID) bool {
	return trustedWindowsDirectoryOwner(sid, current) ||
		sid.IsWellKnown(windows.WinCreatorOwnerSid) ||
		sid.IsWellKnown(windows.WinCreatorOwnerRightsSid)
}
