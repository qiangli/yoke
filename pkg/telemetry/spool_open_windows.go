//go:build windows

package telemetry

import "os"

// Go's write-only append handles have FILE_APPEND_DATA but no GENERIC_WRITE.
// LockFileEx requires GENERIC_READ or GENERIC_WRITE. Retain read access while
// preserving atomic append writes; never seek-and-write on a producer handle.
const spoolAppendFlags = os.O_CREATE | os.O_RDWR | os.O_APPEND
