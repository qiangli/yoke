package spacetime

import "syscall"

func darwinProductVersion() (string, error) {
	return syscall.Sysctl("kern.osproductversion")
}
