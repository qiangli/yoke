//go:build !darwin && !linux

package contain

func sandboxArgv(string, []string) ([]string, error) {
	return nil, &UnavailableError{Level: LevelSandbox, Reason: "no kernel sandbox backend on this platform"}
}
