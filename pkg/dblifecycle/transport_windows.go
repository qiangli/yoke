//go:build windows

package dblifecycle

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// The named pipe DACL admits only the current Windows account and SYSTEM.
// Windows chmod does not enforce 0600, so pipe ACL is the access-control
// boundary and metadata carries no bearer secret on this platform.
func listenOwner(id string) (net.Listener, string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, "", err
	}
	sid := user.User.Sid.String()
	address := `\\.\pipe\bashy-db-` + id
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"
	ln, err := winio.ListenPipe(address, &winio.PipeConfig{SecurityDescriptor: sddl, MessageMode: false})
	return ln, address, err
}

func dialOwner(ctx context.Context, address string) (net.Conn, error) {
	if !strings.HasPrefix(address, `\\.\pipe\bashy-db-s-`) {
		return nil, fmt.Errorf("invalid owner named pipe address")
	}
	return winio.DialPipeContext(ctx, address)
}

func removeOwnerAddress(string) {}

func ownerToken() (string, error) { return "", nil }
