// Package door tells a client where the host's model door is and how to
// authenticate to it. Stdlib only, so anything (the Ollama front door, a
// shell, a test) can import it without pulling in the broker.
package door

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultPort is 24556, "AILLM" on a telephone keypad (22749 is "BASHY").
const DefaultPort = 24556

// PortEnv overrides the port.
const PortEnv = "BASHY_LLM_PORT"

// Port is the door's TCP port.
func Port() int {
	if v := strings.TrimSpace(os.Getenv(PortEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return DefaultPort
}

// BaseURL is the door's loopback base URL.
func BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", Port()) }

// home is the bashy home: $BASHY_HOME or ~/.bashy (never os.UserConfigDir).
func home() (string, error) {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".bashy"), nil
}

// StateDir is where the broker keeps sticky bindings, the audit log and its
// log file.
func StateDir() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "broker"), nil
}

// SocketPath is the owner-only unix socket.
func SocketPath() (string, error) {
	d, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "door.sock"), nil
}

// TokenPath is the owner's bearer token — the same file cligw has always
// used, so one token opens the one door.
func TokenPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "cligw", "token"), nil
}

// Token reads the owner token, creating it (0600 in a 0700 directory) on
// first use.
func Token() (string, error) {
	path, err := TokenPath()
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(data)); t != "" {
			return t, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// OllamaHost is the OLLAMA_HOST value that points an unmodified Ollama
// client at the door: the token rides in the URL path (the client preserves
// a path prefix), and so does the caller's session, when it has one.
func OllamaHost() (string, error) {
	t, err := Token()
	if err != nil {
		return "", err
	}
	u := BaseURL() + "/k/" + t
	if s := strings.TrimSpace(os.Getenv("BASHY_MODEL_SESSION")); s != "" {
		u += "/s/" + s
	}
	return u, nil
}
