package todo

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const windowsOwnerNamespace = ".windows-owners"

// OwnerSubdir locates an owner's personal store. Unlike SanitizeOwner, its
// result may be nested. Windows-unsafe identities use a reserved namespace;
// their original bytes remain the identity, never a lossy filename rewrite.
// Other platforms and ordinary existing owner paths keep their layout.
func OwnerSubdir(owner string) string { return ownerSubdirForOS(owner, runtime.GOOS) }

func ownerSubdirForOS(owner, goos string) string {
	legacy := SanitizeOwner(owner)
	if goos != "windows" {
		return legacy
	}
	if strings.TrimSpace(owner) == "" {
		owner = DefaultOwner
	}
	if windowsOwnerSegment(owner) && !strings.EqualFold(legacy, windowsOwnerNamespace) {
		return legacy
	}
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(windowsOwnerNamespace, hex.EncodeToString(sum[:]))
}

func windowsOwnerSegment(s string) bool {
	if s == "" || s == "." || s == ".." || !utf8.ValidString(s) || len(utf16.Encode([]rune(s))) > 255 || strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return false
	}
	for _, r := range s {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return false
		}
	}
	// Windows device names remain reserved with an extension; spaces before
	// that extension do not turn a device into an ordinary directory.
	base, _, _ := strings.Cut(s, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	if len(base) >= 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		switch base[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return false
		}
	}
	return true
}
