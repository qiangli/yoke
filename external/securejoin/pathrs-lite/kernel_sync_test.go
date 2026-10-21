package pathrs

import (
	"os"
	"strings"
	"testing"
)

// TestKernelCopiesInSync pins that the shared kernel helpers in this
// package and in procfs differ only in their package clause.
func TestKernelCopiesInSync(t *testing.T) {
	a, err := os.ReadFile("kernel_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("procfs/kernel_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Replace(string(b), "\npackage procfs\n", "\npackage pathrs\n", 1)
	if got != string(a) {
		t.Fatal("kernel_linux.go and procfs/kernel_linux.go diverged; keep the copies identical")
	}
}
