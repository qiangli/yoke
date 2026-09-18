package spacetime

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
)

var placeSignals = networkPlaceSignals

func probePlaceID() (string, error) {
	signals, err := placeSignals()
	if err != nil {
		return "", ErrNotApplicable
	}
	signals = canonicalSignals(signals)
	if len(signals) == 0 {
		return "", ErrNotApplicable
	}

	// Length-prefix each private input before hashing to avoid ambiguous
	// concatenations. Only the digest crosses the probe boundary.
	h := sha256.New()
	_, _ = h.Write([]byte("bashy-place-v1\x00"))
	for _, signal := range signals {
		_, _ = h.Write([]byte{byte(len(signal) >> 24), byte(len(signal) >> 16), byte(len(signal) >> 8), byte(len(signal))})
		_, _ = h.Write([]byte(signal))
	}
	return "p" + hex.EncodeToString(h.Sum(nil)), nil
}

func networkPlaceSignals() ([]string, error) {
	signals, err := gatewayHardwareSignals()
	if err != nil {
		signals = nil
	}
	signals = append(signals, dnsSearchSignals("/etc/resolv.conf")...)
	if len(signals) == 0 {
		return nil, ErrNotApplicable
	}
	return signals, nil
}

func dnsSearchSignals(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(strings.SplitN(sc.Text(), "#", 2)[0])
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "domain":
			out = append(out, "dns:"+strings.ToLower(fields[1]))
		case "search":
			for _, suffix := range fields[1:] {
				out = append(out, "dns:"+strings.ToLower(suffix))
			}
		}
	}
	return out
}

func canonicalSignals(signals []string) []string {
	seen := make(map[string]bool, len(signals))
	out := make([]string, 0, len(signals))
	for _, signal := range signals {
		signal = strings.TrimSpace(signal)
		if signal != "" && !seen[signal] {
			seen[signal] = true
			out = append(out, signal)
		}
	}
	sort.Strings(out)
	return out
}
