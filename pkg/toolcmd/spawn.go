package toolcmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// spawnSelf starts bashy's own binary as the detached job runner
// (`<exe> tool cmd _job <id>`), stdio to the job dir, in its own session so
// it outlives the caller. It is never reached from a unit test: tests
// replace spawnJob.
func spawnSelf(id, dir string) (int, error) {
	if jobSelfArgv == nil {
		return 0, errors.New("job runner entry point not mounted")
	}
	argv, err := jobSelfArgv()
	if err != nil {
		return 0, err
	}
	argv = append(argv, "_job", id)
	logf, err := os.OpenFile(filepath.Join(dir, "runner.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdout, c.Stderr, c.Stdin = logf, logf, nil
	c.SysProcAttr = detachAttr()
	if err := c.Start(); err != nil {
		return 0, err
	}
	pid := c.Process.Pid
	_ = c.Process.Release()
	return pid, nil
}
