//go:build windows

package privtest

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// check reads the file's owner and DACL (inherited entries included: a
// private fixture under a profile temp dir is private by inheritance) and
// hands them to evalDACL together with the executing process token's user.
func check(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	tok, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	ownerStr := ""
	if owner != nil {
		ownerStr = owner.String()
	}
	var aces []ace
	if dacl != nil {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var a *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &a); err != nil {
				return err
			}
			e := ace{typ: a.Header.AceType, flags: a.Header.AceFlags}
			if e.typ == aceAccessAllowed {
				e.sid = (*windows.SID)(unsafe.Pointer(&a.SidStart)).String()
			}
			aces = append(aces, e)
		}
	}
	return evalDACL(ownerStr, dacl == nil, aces, tok.User.Sid.String())
}
