//go:build darwin

package spacetime

import (
	"bytes"
	"encoding/hex"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func gatewayHardwareSignals() ([]string, error) {
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}

	var gateways [][]byte
	for _, message := range messages {
		rm, ok := message.(*route.RouteMessage)
		if !ok || rm.Flags&unix.RTF_GATEWAY == 0 || !isDefaultRoute(rm.Addrs) {
			continue
		}
		if gateway := inetBytes(addrAt(rm.Addrs, unix.RTAX_GATEWAY)); len(gateway) > 0 {
			gateways = append(gateways, gateway)
		}
	}
	for _, message := range messages {
		rm, ok := message.(*route.RouteMessage)
		if !ok || rm.Flags&unix.RTF_LLINFO == 0 {
			continue
		}
		dst := inetBytes(addrAt(rm.Addrs, unix.RTAX_DST))
		link, ok := addrAt(rm.Addrs, unix.RTAX_GATEWAY).(*route.LinkAddr)
		if !ok || len(link.Addr) == 0 {
			continue
		}
		for _, gateway := range gateways {
			if bytes.Equal(dst, gateway) {
				return []string{"gateway-mac:" + hex.EncodeToString(link.Addr)}, nil
			}
		}
	}
	return nil, ErrNotApplicable
}

func addrAt(addrs []route.Addr, index int) route.Addr {
	if index < 0 || index >= len(addrs) {
		return nil
	}
	return addrs[index]
}

func inetBytes(addr route.Addr) []byte {
	switch a := addr.(type) {
	case *route.Inet4Addr:
		return a.IP[:]
	case *route.Inet6Addr:
		return a.IP[:]
	default:
		return nil
	}
}

func isDefaultRoute(addrs []route.Addr) bool {
	dst := inetBytes(addrAt(addrs, unix.RTAX_DST))
	if len(dst) == 0 {
		return true
	}
	for _, b := range dst {
		if b != 0 {
			return false
		}
	}
	return true
}
