package chat

import "testing"

func TestCoachExposesExplicitUnsafePermissionOptIn(t *testing.T) {
	cmd := NewCoachCmd()
	if cmd.Flags().Lookup("yolo") == nil {
		t.Fatal("coach must expose --yolo for an unattended acting session")
	}
}
