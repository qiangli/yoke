//go:build windows

package ask

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCheckParentDirAcceptsPrivateWindowsTempDir(t *testing.T) {
	dir := t.TempDir()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o777 {
		t.Logf("Windows reported mode %#o; DACL remains authoritative", fi.Mode().Perm())
	}
	if err := checkParentDir(dir); err != nil {
		t.Fatalf("private Windows temp directory rejected: %v", err)
	}
}

func TestCheckParentDirRejectsBroadWindowsACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "broad")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	sd, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	oldDACL, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(world),
		},
	}}, oldDACL)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}

	err = checkParentDir(dir)
	if err == nil || !strings.Contains(err.Error(), "Windows ACL grants another principal") {
		t.Fatalf("broad Windows ACL accepted: %v", err)
	}
}
