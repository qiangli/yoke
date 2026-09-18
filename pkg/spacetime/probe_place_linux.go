//go:build linux

package spacetime

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"strings"
)

func gatewayHardwareSignals() ([]string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var gateway string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err == nil && len(raw) == net.IPv4len {
			gateway = net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
			break
		}
	}
	if gateway == "" {
		return nil, ErrNotApplicable
	}
	return linuxARPFor(gateway)
}

func linuxARPFor(gateway string) ([]string, error) {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 4 && fields[0] == gateway && fields[3] != "00:00:00:00:00:00" {
			return []string{"gateway-mac:" + strings.ToLower(fields[3])}, nil
		}
	}
	return nil, ErrNotApplicable
}
