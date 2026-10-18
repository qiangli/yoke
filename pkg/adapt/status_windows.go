package adapt

import "os/exec"

func signalStatus(e *exec.ExitError) int { return 1 }
