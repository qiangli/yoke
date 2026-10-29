//go:build darwin

package contain

import (
	"os/exec"
	"strings"
)

func sandboxArgv(dir string, argv []string) ([]string, error) {
	sb, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, &UnavailableError{Level: LevelSandbox, Reason: "sandbox-exec not found"}
	}
	if strings.ContainsAny(dir, "\"\\") {
		return nil, &UnavailableError{Level: LevelSandbox, Reason: "clone path not representable in profile"}
	}
	profile := `(version 1)
(deny default)
(allow process-fork)
(allow process-exec)
(allow signal (target self))
(allow sysctl-read)
(allow mach-lookup)
(allow file-read* (subpath "/usr") (subpath "/bin") (subpath "/System") (subpath "/Library") (subpath "/private/var/db") (subpath "/dev") (literal "/") (literal "/private") (literal "/private/var") (literal "/var"))
(allow file-read* file-write* (subpath "` + dir + `"))
(allow file-write* (literal "/dev/null") (literal "/dev/tty"))
(allow network* (local ip "localhost:*") (remote ip "localhost:*") (local unix-socket))
`
	return append([]string{sb, "-p", profile}, argv...), nil
}
