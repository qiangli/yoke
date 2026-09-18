// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package cmake

import "testing"

// TestAsset pins the per-platform archive + entrypoint mapping — including the
// Windows leg, which can't be exec-tested on a non-Windows runner but is the
// product. A wrong archive name or entrypoint here = a broken fetch on that host.
func TestAsset(t *testing.T) {
	const v = "4.3.3"
	cases := []struct{ plat, archive, entry string }{
		{"darwin/arm64", "cmake-4.3.3-macos-universal.tar.gz", "cmake-4.3.3-macos-universal/CMake.app/Contents/bin/cmake"},
		{"darwin/amd64", "cmake-4.3.3-macos-universal.tar.gz", "cmake-4.3.3-macos-universal/CMake.app/Contents/bin/cmake"},
		{"linux/amd64", "cmake-4.3.3-linux-x86_64.tar.gz", "cmake-4.3.3-linux-x86_64/bin/cmake"},
		{"linux/arm64", "cmake-4.3.3-linux-aarch64.tar.gz", "cmake-4.3.3-linux-aarch64/bin/cmake"},
		{"windows/amd64", "cmake-4.3.3-windows-x86_64.zip", "cmake-4.3.3-windows-x86_64/bin/cmake.exe"},
		{"windows/arm64", "cmake-4.3.3-windows-arm64.zip", "cmake-4.3.3-windows-arm64/bin/cmake.exe"},
	}
	for _, c := range cases {
		t.Run(c.plat, func(t *testing.T) {
			archive, entry, err := asset(c.plat, v)
			if err != nil {
				t.Fatalf("asset(%q): %v", c.plat, err)
			}
			if archive != c.archive {
				t.Errorf("archive = %q, want %q", archive, c.archive)
			}
			if entry != c.entry {
				t.Errorf("entrypoint = %q, want %q", entry, c.entry)
			}
		})
	}
	if _, _, err := asset("plan9/mips", v); err == nil {
		t.Error("asset(unsupported platform) = nil error, want error")
	}
}
