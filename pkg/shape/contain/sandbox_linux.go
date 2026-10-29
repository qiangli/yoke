//go:build linux

package contain

import "os/exec"

func sandboxArgv(dir string, argv []string) ([]string, error) {
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, &UnavailableError{Level: LevelSandbox, Reason: "bwrap not found"}
	}
	a := []string{bw, "--die-with-parent", "--unshare-all",
		"--ro-bind", "/usr", "/usr", "--ro-bind-try", "/bin", "/bin", "--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64", "--dev", "/dev", "--proc", "/proc",
		"--bind", dir, dir, "--chdir", dir,
		"--"}
	return append(a, argv...), nil
}
