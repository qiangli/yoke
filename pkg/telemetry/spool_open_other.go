//go:build !windows

package telemetry

import "os"

const spoolAppendFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
