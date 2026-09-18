// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// machineIDFromFiles reads the first readable, non-empty machine-id file.
//
// The contents are hex-encoded rather than used verbatim: /etc/hostid on the BSDs is
// four raw binary bytes, and a raw byte in a store path or a comparison token is a
// portability bug waiting to happen. The value is only ever COMPARED, never
// interpreted, so any injective encoding will do.
func machineIDFromFiles(paths ...string) (string, string, error) {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			continue
		}
		if !isPrintableASCII(v) {
			v = hex.EncodeToString(b)
		}
		return "file:" + v, "file:" + p, nil
	}
	return "", "", fmt.Errorf("none of %s is readable", strings.Join(paths, ", "))
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return len(s) > 0
}
