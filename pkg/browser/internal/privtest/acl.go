package privtest

import "fmt"

// DACL evaluation is kept free of OS types (SIDs travel as strings) so its
// edge cases are unit-testable on every platform; privtest_windows.go only
// reads the real descriptor and feeds it in.

const (
	sidSystem = "S-1-5-18"
	sidAdmins = "S-1-5-32-544"

	aceAccessAllowed = 0x00
	aceAccessDenied  = 0x01
	aceDeniedObject  = 0x06
	aceDeniedCB      = 0x0A
	aceDeniedCBObj   = 0x0C

	aceInheritOnly = 0x08
)

type ace struct {
	typ   byte
	flags byte
	sid   string // meaningful only for aceAccessAllowed
}

// evalDACL decides whether a file with this owner and DACL is private to the
// executing user (me), SYSTEM and Administrators.
//   - the owner must be one of those three;
//   - an inherit-only entry does not apply to the file and is skipped;
//   - an applicable allow entry must target only one of those three;
//   - known deny entries are ignored (they only narrow access);
//   - any other entry type fails closed, since callback/object/compound allow
//     entries grant access and their SID is not checked here;
//   - an applicable allow entry for me specifically must exist;
//   - a NULL DACL (nullDACL) grants everyone access.
func evalDACL(owner string, nullDACL bool, aces []ace, me string) error {
	trusted := func(s string) bool { return s == me || s == sidSystem || s == sidAdmins }
	if !trusted(owner) {
		return fmt.Errorf("owner %s is not the running user, SYSTEM or Administrators", owner)
	}
	if nullDACL {
		return fmt.Errorf("NULL DACL grants everyone access")
	}
	meAllowed := false
	for i, a := range aces {
		if a.flags&aceInheritOnly != 0 {
			continue
		}
		switch a.typ {
		case aceAccessDenied, aceDeniedObject, aceDeniedCB, aceDeniedCBObj:
			continue
		case aceAccessAllowed:
			if !trusted(a.sid) {
				return fmt.Errorf("allow entry %d for %s", i, a.sid)
			}
			if a.sid == me {
				meAllowed = true
			}
		default:
			return fmt.Errorf("entry %d has unhandled type 0x%02x (fail closed)", i, a.typ)
		}
	}
	if !meAllowed {
		return fmt.Errorf("no applicable allow entry for the running user %s", me)
	}
	return nil
}
