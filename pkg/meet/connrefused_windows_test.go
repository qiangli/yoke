//go:build windows

package meet

import "golang.org/x/sys/windows"

// platformRefusal is the errno the platform's network stack actually returns
// for a dial to a port with no listener.
var platformRefusal error = windows.WSAECONNREFUSED
