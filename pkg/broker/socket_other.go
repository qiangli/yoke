//go:build !unix

package broker

import "os"

// ownedBySelf: no unix ownership here; the door opens no unix listener on
// these platforms (peerCredentialsSupported is false).
func ownedBySelf(os.FileInfo) bool { return true }
