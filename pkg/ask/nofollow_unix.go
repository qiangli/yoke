//go:build !windows

package ask

import "syscall"

// noFollow makes an open refuse a symlink, closing the classic shared-directory
// attack: an attacker pre-creates the path as a link into somewhere they can read,
// and the victim writes the secret straight through it.
const noFollow = syscall.O_NOFOLLOW
