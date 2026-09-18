//go:build windows

package ask

// noFollow has no Windows equivalent — there is no O_NOFOLLOW, and the reparse
// point that would be the analogue needs CreateFile with
// FILE_FLAG_OPEN_REPARSE_POINT rather than an open flag.
//
// The exposure it guards against is also much smaller here: the ask root lives
// under the per-user profile directory, which is ACL'd to the user, so there is no
// shared-directory equivalent of /tmp in the default path. Recorded as a known
// asymmetry rather than left as an unexplained zero.
const noFollow = 0
