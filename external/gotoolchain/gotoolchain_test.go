package gotoolchain

import "testing"

func TestWindowsNativePathForGOOS(t *testing.T) {
	tests := []struct {
		name string
		goos string
		in   string
		want string
	}{
		{"msys temp", "windows", "/c/Users/liqiang/AppData/Local/Temp", `C:\Users\liqiang\AppData\Local\Temp`},
		{"shellified go cache", "windows", `\c\Users\liqiang\go\pkg\mod`, `C:\Users\liqiang\go\pkg\mod`},
		{"drive path", "windows", `C:\Users\liqiang\AppData\Local\Temp`, `C:\Users\liqiang\AppData\Local\Temp`},
		{"other os unchanged", "linux", "/c/Users/liqiang/AppData/Local/Temp", "/c/Users/liqiang/AppData/Local/Temp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowsNativePathForGOOS(tt.goos, tt.in); got != tt.want {
				t.Fatalf("windowsNativePathForGOOS(%q, %q) = %q, want %q", tt.goos, tt.in, got, tt.want)
			}
		})
	}
}
