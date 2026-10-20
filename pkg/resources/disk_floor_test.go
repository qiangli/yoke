package resources

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseDiskFloor(t *testing.T) {
	tests := []struct {
		input       string
		wantBytes   uint64
		wantPercent float64
		wantErr     bool
	}{
		{input: "100G", wantBytes: 100 * 1024 * 1024 * 1024, wantPercent: 0},
		{input: "100GiB", wantBytes: 100 * 1024 * 1024 * 1024, wantPercent: 0},
		{input: "100GB", wantBytes: 100 * 1024 * 1024 * 1024, wantPercent: 0},
		{input: "500M", wantBytes: 500 * 1024 * 1024, wantPercent: 0},
		{input: "500MiB", wantBytes: 500 * 1024 * 1024, wantPercent: 0},
		{input: "1T", wantBytes: 1024 * 1024 * 1024 * 1024, wantPercent: 0},
		{input: "10%", wantBytes: 0, wantPercent: 10.0},
		{input: "5.5%", wantBytes: 0, wantPercent: 5.5},
		{input: "100G,10%", wantBytes: 100 * 1024 * 1024 * 1024, wantPercent: 10.0},
		{input: "15%,50GiB", wantBytes: 50 * 1024 * 1024 * 1024, wantPercent: 15.0},
		{input: "", wantErr: true},
		{input: "invalid", wantErr: true},
		{input: "-10G", wantErr: true},
		{input: "0%", wantErr: true},
		{input: "150%", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			cfg, err := ParseDiskFloor(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseDiskFloor(%q) expected error, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDiskFloor(%q) unexpected error: %v", tc.input, err)
			}
			if cfg.MinFreeBytes != tc.wantBytes {
				t.Errorf("ParseDiskFloor(%q) MinFreeBytes = %d, want %d", tc.input, cfg.MinFreeBytes, tc.wantBytes)
			}
			if cfg.MinFreePercent != tc.wantPercent {
				t.Errorf("ParseDiskFloor(%q) MinFreePercent = %f, want %f", tc.input, cfg.MinFreePercent, tc.wantPercent)
			}
		})
	}
}

func TestCheckDiskFloorInjectedNumbers(t *testing.T) {
	const giB = 1024 * 1024 * 1024
	disks := []Disk{
		{
			Mount:       "/",
			Device:      "/dev/disk1s1",
			FSType:      "apfs",
			TotalBytes:  460 * giB,
			UsedBytes:   435 * giB,
			FreeBytes:   25 * giB,
			UsedPercent: 94.5,
		},
		{
			Mount:       "/System/Volumes/xarts",
			Device:      "/dev/disk1s2",
			FSType:      "apfs",
			TotalBytes:  500 * 1024 * 1024,
			UsedBytes:   20 * 1024 * 1024,
			FreeBytes:   480 * 1024 * 1024,
			UsedPercent: 4.0,
		},
	}

	t.Run("fails when free space is below byte floor", func(t *testing.T) {
		cfg := DiskFloorConfig{MinFreeBytes: 100 * giB}
		err := CheckDiskFloor(disks, cfg)
		if err == nil {
			t.Fatal("CheckDiskFloor expected error when free space is 25GiB < 100GiB floor, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "disk floor alarm:") {
			t.Errorf("expected 'disk floor alarm:' prefix, got: %s", msg)
		}
		if !strings.Contains(msg, `mount "/"`) {
			t.Errorf("expected mount / in alarm, got: %s", msg)
		}
		if !strings.Contains(msg, "below 100GiB floor") && !strings.Contains(msg, "below floor") {
			t.Errorf("expected floor in alarm, got: %s", msg)
		}
		if strings.Contains(msg, "\n") {
			t.Errorf("alarm must be a single line, got newline: %q", msg)
		}
	})

	t.Run("passes when free space is above byte floor", func(t *testing.T) {
		cfg := DiskFloorConfig{MinFreeBytes: 20 * giB}
		if err := CheckDiskFloor(disks, cfg); err != nil {
			t.Fatalf("CheckDiskFloor unexpected error for 20GiB floor: %v", err)
		}
	})

	t.Run("fails when free space is below percent floor", func(t *testing.T) {
		// 25GiB free of 460GiB is ~5.43% free.
		cfg := DiskFloorConfig{MinFreePercent: 10.0}
		err := CheckDiskFloor(disks, cfg)
		if err == nil {
			t.Fatal("CheckDiskFloor expected error when free percent is 5.4% < 10% floor, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "disk floor alarm:") {
			t.Errorf("expected 'disk floor alarm:' prefix, got: %s", msg)
		}
	})

	t.Run("passes when free space is above percent floor", func(t *testing.T) {
		cfg := DiskFloorConfig{MinFreePercent: 5.0}
		if err := CheckDiskFloor(disks, cfg); err != nil {
			t.Fatalf("CheckDiskFloor unexpected error for 5%% floor: %v", err)
		}
	})

	t.Run("fails when both floors specified and one fails", func(t *testing.T) {
		cfg := DiskFloorConfig{MinFreeBytes: 20 * giB, MinFreePercent: 10.0}
		if err := CheckDiskFloor(disks, cfg); err == nil {
			t.Fatal("expected error when percent floor breached")
		}
	})

	t.Run("ignores small partition when checking all mounts with large byte floor", func(t *testing.T) {
		// The 500MB xarts partition should not trigger 100GiB alarm by itself
		// if the root mount has sufficient space.
		highDisks := []Disk{
			{
				Mount:      "/",
				TotalBytes: 500 * giB,
				FreeBytes:  250 * giB,
			},
			{
				Mount:      "/System/Volumes/xarts",
				TotalBytes: 500 * 1024 * 1024,
				FreeBytes:  480 * 1024 * 1024,
			},
		}
		cfg := DiskFloorConfig{MinFreeBytes: 100 * giB}
		if err := CheckDiskFloor(highDisks, cfg); err != nil {
			t.Fatalf("CheckDiskFloor should pass for 250GiB free on root: %v", err)
		}
	})

	t.Run("mount filter targets specific mount", func(t *testing.T) {
		cfg := DiskFloorConfig{MinFreeBytes: 100 * giB, Mount: "/System/Volumes/xarts"}
		// When specifically targeting xarts, it should fail the 100GiB floor.
		if err := CheckDiskFloor(disks, cfg); err == nil {
			t.Fatal("expected failure when specifically targeting xarts with 100GiB floor")
		}
	})

	t.Run("mount filter reports unknown mount", func(t *testing.T) {
		cfg := DiskFloorConfig{MinFreeBytes: 100 * giB, Mount: "/nonexistent"}
		if err := CheckDiskFloor(disks, cfg); err == nil {
			t.Fatal("expected error for nonexistent mount filter")
		}
	})
}

func TestSystemCommandMinFreeFlag(t *testing.T) {
	cmd := NewSystemCommand()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--interval", "0", "--min-free", "10000000G"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit for impossible 10000000G floor")
	}
	if !strings.Contains(err.Error(), "disk floor alarm:") {
		t.Fatalf("expected error to contain 'disk floor alarm:', got: %q", err.Error())
	}
}
