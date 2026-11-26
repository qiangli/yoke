// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build linux

package artifact

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readBoundedProcFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("proc file exceeds bound")
	}
	return b, err
}

func lookupProcessStartID(pid int) (string, error) {
	if pid <= 0 || pid > 1<<31-1 {
		return "", os.ErrInvalid
	}
	boot, err := readBoundedProcFile("/proc/sys/kernel/random/boot_id", 128)
	if err != nil {
		return "", err
	}
	stat, err := readBoundedProcFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"), 16384)
	if err != nil {
		return "", err
	}
	first, last := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if first < 0 || last < first {
		return "", fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(stat[last+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("short proc stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(string(boot))
	if bootID == "" {
		return "", fmt.Errorf("empty boot_id")
	}
	return fmt.Sprintf("linux:%s:%d", bootID, start), nil
}
