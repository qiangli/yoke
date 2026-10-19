package indexer

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestIndexOwnerACL(t *testing.T) {
	m, _ := fixture(t)
	child := filepath.Join(m.dir, "owner-check")
	if e := os.WriteFile(child, []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{m.dir, child} {
		sd, e := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if e != nil {
			t.Fatal(e)
		}
		acl, _, e := sd.DACL()
		if e != nil || acl == nil || acl.AceCount == 0 {
			t.Fatalf("%s: expected owner ACEs: %v; %s", path, e, sd.String())
		}
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if e := windows.GetAce(acl, i, &ace); e != nil {
				t.Fatal(e)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
				t.Fatalf("%s: non-owner access: %s", path, sd.String())
			}
		}
	}
}
