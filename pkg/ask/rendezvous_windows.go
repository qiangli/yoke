//go:build windows

package ask

// listenSocket / sendSocket are unavailable on Windows.
//
// Go can bind an AF_UNIX socket on recent Windows builds, but there is no
// SO_PEERCRED equivalent — so the peer-uid check that makes the socket channel
// trustworthy could not run. Rather than ship a socket whose authorization step is
// a stub, Windows uses the file channel, where protection comes from the ACL on
// the per-user profile directory the ask root lives in.
func listenSocket(string) (listener, error) { return nil, errNoSocket }

func sendSocket(string, []byte) error { return errNoSocket }
