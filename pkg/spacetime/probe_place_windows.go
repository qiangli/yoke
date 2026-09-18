//go:build windows

package spacetime

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func gatewayHardwareSignals() ([]string, error) {
	size := uint32(15 * 1024)
	for attempts := 0; attempts < 3; attempts++ {
		buf := make([]byte, size)
		head := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(
			windows.AF_UNSPEC,
			windows.GAA_FLAG_INCLUDE_GATEWAYS,
			0,
			head,
			&size,
		)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return nil, err
		}

		var out []string
		for adapter := head; adapter != nil; adapter = adapter.Next {
			if adapter.OperStatus != windows.IfOperStatusUp || adapter.FirstGatewayAddress == nil {
				continue
			}
			if suffix := strings.TrimSpace(windows.UTF16PtrToString(adapter.DnsSuffix)); suffix != "" {
				out = append(out, "dns:"+strings.ToLower(suffix))
			}
			for suffix := adapter.FirstDnsSuffix; suffix != nil; suffix = suffix.Next {
				value := strings.TrimSpace(windows.UTF16ToString(suffix.String[:]))
				if value != "" {
					out = append(out, "dns:"+strings.ToLower(value))
				}
			}
		}
		if len(out) == 0 {
			return nil, ErrNotApplicable
		}
		return out, nil
	}
	return nil, ErrNotApplicable
}
