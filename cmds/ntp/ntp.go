package ntpcmd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"time"

	"github.com/qiangli/coreutils/tool"
)

var (
	ntpTool = &tool.Tool{
		Name:     "ntp",
		Synopsis: "Query an NTP server with a pure-Go SNTP client.",
		Usage:    "ntp [--server HOST] [--timeout DUR] [--check] [--max-skew DUR] [--json]",
	}
	sntpTool = &tool.Tool{
		Name:     "sntp",
		Synopsis: "Alias for ntp.",
		Usage:    "sntp [--server HOST] [--timeout DUR] [--check] [--max-skew DUR] [--json]",
	}
)

func init() {
	ntpTool.Run = func(rc *tool.RunContext, args []string) int { return run(rc, ntpTool, args) }
	sntpTool.Run = func(rc *tool.RunContext, args []string) int { return run(rc, sntpTool, args) }
	tool.Register(ntpTool)
	tool.Register(sntpTool)
}

type result struct {
	Server string        `json:"server"`
	Time   time.Time     `json:"time"`
	Offset time.Duration `json:"-"`
	RTT    time.Duration `json:"-"`
}

func run(rc *tool.RunContext, t *tool.Tool, args []string) int {
	fs := tool.NewFlags(t.Name)
	server := fs.String("server", "pool.ntp.org", "NTP server host")
	timeout := fs.String("timeout", "3s", "network timeout")
	check := fs.Bool("check", false, "exit non-zero when clock skew exceeds --max-skew")
	maxSkew := fs.String("max-skew", "1s", "maximum allowed absolute offset for --check")
	jsonOut := fs.Bool("json", false, "write a machine-readable JSON envelope")
	operands, code := tool.Parse(rc, t, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) != 0 {
		return tool.UsageError(rc, t, "extra operand %q", operands[0])
	}
	to, err := time.ParseDuration(*timeout)
	if err != nil || to <= 0 {
		return tool.UsageError(rc, t, "invalid timeout %q", *timeout)
	}
	skew, err := time.ParseDuration(*maxSkew)
	if err != nil || skew < 0 {
		return tool.UsageError(rc, t, "invalid max-skew %q", *maxSkew)
	}
	res, err := query(rc.Ctx, *server, to)
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: %v\n", t.Name, err)
		return 1
	}
	if *jsonOut {
		body := map[string]any{
			"ok":             true,
			"server":         res.Server,
			"time":           res.Time.Format(time.RFC3339Nano),
			"offset_seconds": res.Offset.Seconds(),
			"offset":         res.Offset.String(),
			"rtt_seconds":    res.RTT.Seconds(),
			"check":          *check,
			"max_skew":       skew.String(),
		}
		if err := json.NewEncoder(rc.Out).Encode(body); err != nil {
			fmt.Fprintf(rc.Err, "%s: %v\n", t.Name, err)
			return 1
		}
	} else {
		fmt.Fprintf(rc.Out, "%s offset=%s rtt=%s server=%s\n", res.Time.Format(time.RFC3339Nano), res.Offset, res.RTT, res.Server)
	}
	if *check && absDuration(res.Offset) > skew {
		fmt.Fprintf(rc.Err, "%s: clock offset %s exceeds max skew %s\n", t.Name, res.Offset, skew)
		return 1
	}
	return 0
}

func query(ctx context.Context, server string, timeout time.Duration) (result, error) {
	addr := server
	if _, _, err := net.SplitHostPort(server); err != nil {
		addr = net.JoinHostPort(server, "123")
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return result{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return result{}, err
	}
	var req [48]byte
	req[0] = 0x23 // LI=0, VN=4, mode=3 client.
	t1 := time.Now()
	putTimestamp(req[40:48], t1)
	if _, err := conn.Write(req[:]); err != nil {
		return result{}, err
	}
	var resp [48]byte
	n, err := conn.Read(resp[:])
	if err != nil {
		return result{}, err
	}
	t4 := time.Now()
	r, err := parseResponse(resp[:n], t1, t4)
	if err != nil {
		return result{}, err
	}
	r.Server = server
	return r, nil
}

func parseResponse(packet []byte, t1, t4 time.Time) (result, error) {
	if len(packet) < 48 {
		return result{}, fmt.Errorf("short NTP packet")
	}
	mode := packet[0] & 0x7
	if mode != 4 && mode != 5 {
		return result{}, fmt.Errorf("unexpected NTP mode %d", mode)
	}
	if packet[1] == 0 {
		return result{}, fmt.Errorf("unspecified NTP stratum")
	}
	t2 := readTimestamp(packet[32:40])
	t3 := readTimestamp(packet[40:48])
	if t2.IsZero() || t3.IsZero() {
		return result{}, fmt.Errorf("missing NTP timestamps")
	}
	offset := (t2.Sub(t1) + t3.Sub(t4)) / 2
	rtt := t4.Sub(t1) - t3.Sub(t2)
	return result{Time: t3, Offset: offset, RTT: rtt}, nil
}

const ntpUnixOffset = 2208988800

func putTimestamp(dst []byte, t time.Time) {
	sec := uint64(t.Unix() + ntpUnixOffset)
	frac := uint64(t.Nanosecond()) * (1 << 32) / 1e9
	binary.BigEndian.PutUint32(dst[0:4], uint32(sec))
	binary.BigEndian.PutUint32(dst[4:8], uint32(frac))
}

func readTimestamp(src []byte) time.Time {
	sec := int64(binary.BigEndian.Uint32(src[0:4])) - ntpUnixOffset
	frac := int64(binary.BigEndian.Uint32(src[4:8]))
	nsec := frac * 1e9 / (1 << 32)
	if sec == -ntpUnixOffset && frac == 0 {
		return time.Time{}
	}
	return time.Unix(sec, nsec).UTC()
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		if d == time.Duration(math.MinInt64) {
			return time.Duration(math.MaxInt64)
		}
		return -d
	}
	return d
}
