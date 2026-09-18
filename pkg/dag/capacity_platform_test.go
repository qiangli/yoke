package dag

import "testing"

func TestCapacityUnsupportedPlatformsNeverAdvertiseGuardedExecution(t *testing.T) {
	for _, platform := range []string{"windows", "aix", "plan9"} {
		if capacityPlatformSupportsExecution(platform) {
			t.Fatalf("unsupported execution platform advertised: %s", platform)
		}
	}
	if !capacityPlatformSupportsExecution("linux") || !capacityPlatformSupportsExecution("darwin") {
		t.Fatal("supported guarded platform refused")
	}
}
