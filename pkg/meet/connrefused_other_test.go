//go:build !windows

package meet

import "syscall"

// platformRefusal is the errno the platform's network stack actually returns
// for a dial to a port with no listener.
var platformRefusal error = syscall.ECONNREFUSED
