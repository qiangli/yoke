package recall

import "runtime"

// osGOOS is a variable rather than a direct runtime.GOOS reference so scope
// filtering can be exercised for other platforms in tests without build tags.
var osGOOS = runtime.GOOS
