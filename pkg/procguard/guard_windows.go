//go:build windows

package procguard

import (
	"fmt"
	"os/exec"
)

type Guard struct{}

func ContainsSessionEscapes() bool { return false }

func Arm(_ *exec.Cmd) (*Guard, error) {
	return nil, fmt.Errorf("procguard: kill-on-parent-exit is unsupported on windows")
}

func (*Guard) Started(error) {}
func (*Guard) Disarm()       {}
