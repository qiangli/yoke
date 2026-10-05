package pwsh

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/muslrt"
)

// The arm64 musl route is the framework-dependent archive on a pinned runtime;
// the x64 one is self-contained.
func TestMuslRoutesArePinnedPerArch(t *testing.T) {
	arm, err := assetFor(DefaultVersion, "linux", "arm64", true)
	if err != nil || !arm.fxdependent {
		t.Fatalf("linux/arm64 musl = %+v, %v; want the framework-dependent archive", arm, err)
	}
	rt, ok := dotnetRuntime[platformKey(DefaultVersion, "linux", "arm64", true)]
	if !ok || len(rt.sha256) != 64 || !strings.Contains(rt.url, "linux-musl-arm64") || !strings.Contains(rt.url, rt.version) {
		t.Fatalf("arm64 .NET runtime pin = %+v", rt)
	}
	x64, err := assetFor(DefaultVersion, "linux", "amd64", true)
	if err != nil || x64.fxdependent {
		t.Fatalf("linux/amd64 musl = %+v, %v; want the self-contained archive", x64, err)
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		libs, err := muslLibs(goarch)
		if err != nil || len(libs) != 4 {
			t.Fatalf("muslLibs(%s) = %d, %v", goarch, len(libs), err)
		}
		for _, l := range libs {
			if len(l.Pkg.SHA256) != 64 || l.Pkg.License == "" || !strings.HasSuffix(l.Member, l.Soname) {
				t.Fatalf("%s lib pin malformed: %+v", goarch, l)
			}
		}
		if p := muslrt.LibPslNative[goarch]; len(p.SHA256) != 64 || p.License != "MIT" {
			t.Fatalf("%s libpsl-native pin = %+v", goarch, p)
		}
	}
}

func TestLaunchEnvironPrependsLibraryPath(t *testing.T) {
	l := Launch{Argv: []string{"/x/dotnet", "/x/pwsh.dll"}, Env: []string{"LD_LIBRARY_PATH=/c/libs", "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1"}}
	got := strings.Join(l.Environ([]string{"A=1", "LD_LIBRARY_PATH=/usr/x", "POWERSHELL_UPDATECHECK=Default"}), "\n")
	for _, want := range []string{"A=1", "LD_LIBRARY_PATH=/c/libs:/usr/x", "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1", "POWERSHELL_UPDATECHECK=Off", "POWERSHELL_TELEMETRY_OPTOUT=1"} {
		if !strings.Contains(got+"\n", want+"\n") {
			t.Fatalf("Environ missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "LD_LIBRARY_PATH=") != 1 || strings.Count(got, "POWERSHELL_UPDATECHECK=") != 1 {
		t.Fatalf("Environ duplicates a variable:\n%s", got)
	}
	// Already first on the path: left alone, never doubled.
	again := l.Overrides([]string{"LD_LIBRARY_PATH=/c/libs:/usr/x"})
	if strings.Join(again, " ") != "POWERSHELL_TELEMETRY_OPTOUT=1 POWERSHELL_UPDATECHECK=Off LD_LIBRARY_PATH=/c/libs:/usr/x DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1" {
		t.Fatalf("Overrides = %q", again)
	}
	if got := (Launch{Argv: []string{"/x/dotnet", "/x/pwsh.dll"}}).FenceArgv(); strings.Join(got, " ") != "/x/dotnet /x/pwsh.dll -NoLogo -NoProfile -NonInteractive" {
		t.Fatalf("FenceArgv = %q", got)
	}
}
