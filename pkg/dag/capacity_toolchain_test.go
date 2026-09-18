package dag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func declaredFixture(t *testing.T) CapacityExecutable {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compiler.exe")
	body := []byte("fixture compiler bytes\n")
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	return CapacityExecutable{Path: path, SHA256: hex.EncodeToString(hash[:])}
}
func TestCapacityExecutableVerifiesAndRejectsReplacement(t *testing.T) {
	entry := declaredFixture(t)
	entry.SHA256 = strings.ToUpper(entry.SHA256)
	got, err := VerifyCapacityExecutables(context.Background(), []CapacityExecutable{entry})
	if err != nil || len(got) != 1 || got[0].SHA256 != strings.ToLower(entry.SHA256) {
		t.Fatal(got, err)
	}
	if err := os.WriteFile(entry.Path, []byte("replacement compiler bytes\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCapacityExecutables(context.Background(), []CapacityExecutable{entry}); err == nil {
		t.Fatal("changed executable accepted")
	}
}
func TestCapacityExecutableDeclarationBounds(t *testing.T) {
	entry := declaredFixture(t)
	for _, tc := range []struct {
		name    string
		entries []CapacityExecutable
	}{
		{"relative", []CapacityExecutable{{Path: "compiler", SHA256: entry.SHA256}}},
		{"missing digest", []CapacityExecutable{{Path: entry.Path}}},
		{"invalid digest", []CapacityExecutable{{Path: entry.Path, SHA256: strings.Repeat("z", 64)}}},
		{"duplicate", []CapacityExecutable{entry, entry}},
		{"too many", make([]CapacityExecutable, 9)},
		{"directory", []CapacityExecutable{{Path: filepath.Dir(entry.Path), SHA256: entry.SHA256}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyCapacityExecutables(context.Background(), tc.entries); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
	if runtime.GOOS == "windows" {
		unsupported := entry.Path + ".cmd"
		if err := os.Rename(entry.Path, unsupported); err != nil {
			t.Fatal(err)
		}
		entry.Path = unsupported
	} else if err := os.Chmod(entry.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCapacityExecutables(context.Background(), []CapacityExecutable{entry}); err == nil {
		t.Fatal("non-native or non-executable artifact accepted")
	}
}
func TestCapacityExecutableSymlinkIdentityAndByteLimits(t *testing.T) {
	entry := declaredFixture(t)
	alias := entry
	alias.Path = entry.Path + "-alias.exe"
	if err := os.Symlink(entry.Path, alias.Path); err != nil {
		t.Skip(err)
	}
	if _, err := VerifyCapacityExecutables(context.Background(), []CapacityExecutable{alias}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCapacityExecutables(context.Background(), []CapacityExecutable{entry, alias}); err == nil {
		t.Fatal("aliased duplicate accepted")
	}

}
func TestCapacityExecutableByteLimits(t *testing.T) {
	entry := declaredFixture(t)
	if _, _, err := fingerprintCapacityExecutable(context.Background(), entry.Path, 1); err == nil {
		t.Fatal("total byte budget ignored")
	}
	if err := os.Truncate(entry.Path, capacityExecutableBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCapacityExecutables(context.Background(), []CapacityExecutable{entry}); err == nil {
		t.Fatal("oversize executable accepted")
	}
}

func TestCapacityExecutableCancelledAndWorkerBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyCapacityExecutables(ctx, []CapacityExecutable{declaredFixture(t)}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Simulate unavailable filesystem workers without depending on an actual
	// network mount or leaving a blocked test subprocess behind.
	for i := 0; i < cap(capacityFingerprintWorkers); i++ {
		capacityFingerprintWorkers <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(capacityFingerprintWorkers); i++ {
			<-capacityFingerprintWorkers
		}
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := VerifyCapacityExecutables(ctx, []CapacityExecutable{declaredFixture(t)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestCapacityExecutablePlatformMode(t *testing.T) {
	for _, tc := range []struct {
		platform, path string
		mode           os.FileMode
		want           bool
	}{
		{"windows", "compiler.EXE", 0666, true},
		{"windows", "compiler.com", 0600, true},
		{"windows", "compiler.exe", os.ModeDir | 0777, false},
		{"windows", "script.cmd", 0777, false},
		{"windows", "script.bat", 0777, false},
		{"windows", "compiler", 0777, false},
		{"linux", "compiler", 0700, true},
		{"darwin", "compiler.exe", 0600, false},
		{"linux", "compiler", os.ModeNamedPipe | 0700, false},
	} {
		if got := capacityExecutableMode(tc.platform, tc.path, tc.mode); got != tc.want {
			t.Errorf("%s %s %v: got %t want %t", tc.platform, tc.path, tc.mode, got, tc.want)
		}
	}
}
