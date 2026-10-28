//go:build bashy_guard_fake

package guard

// fakeBuild is true only in a binary built with -tags bashy_guard_fake, for
// process-level fixtures that drive the real CLI. Never a release build.
const fakeBuild = true
