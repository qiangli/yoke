package lens

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCmdGoldens(t *testing.T) {
	cases := []struct {
		cmd      string
		outFile  string
		jsonFile string
	}{
		{"cmd:ps", "ps.out", "ps.json"},
		{"cmd:df", "df.out", "df.json"},
		{"cmd:ls-l", "ls-l.out", "ls-l.json"},
		{"cmd:du", "du.out", "du.json"},
		{"cmd:wc", "wc.out", "wc.json"},
		{"cmd:stat", "stat.out", "stat.json"},
		{"cmd:id", "id.out", "id.json"},
		{"cmd:uname", "uname.out", "uname.json"},
		{"cmd:tar-tv", "tar-tv.out", "tar-tv.json"},
		{"cmd:ip-route", "ip-route.out", "ip-route.json"},
		{"cmd:netstat-rn", "netstat-rn.out", "netstat-rn.json"},
	}

	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			input := read(t, tc.outFile)
			res := convert(t, tc.cmd, "json", input)
			golden(t, tc.jsonFile, res.Data)
		})
	}
}

func TestCmdRowRegistration(t *testing.T) {
	cmds := []string{
		"cmd:ps", "cmd:df", "cmd:ls-l", "cmd:du", "cmd:wc",
		"cmd:stat", "cmd:id", "cmd:uname", "cmd:tar-tv",
		"cmd:ip-route", "cmd:netstat-rn",
	}

	for _, c := range cmds {
		row, err := Route(c, "json")
		if err != nil {
			t.Errorf("expected route for %s -> json, got error: %v", c, err)
			continue
		}
		if row.Fidelity != Lossy {
			t.Errorf("expected Lossy fidelity for %s, got %v", c, row.Fidelity)
		}
		if row.Via() != "go" {
			t.Errorf("expected built-in 'go' via for %s, got %s", c, row.Via())
		}
	}
}

func TestCmdEmptyInputs(t *testing.T) {
	// Table/line commands return empty JSON array on empty input
	tableCmds := []string{
		"cmd:ps", "cmd:df", "cmd:ls-l", "cmd:du", "cmd:wc", "cmd:tar-tv",
		"cmd:ip-route", "cmd:netstat-rn",
	}
	for _, cmd := range tableCmds {
		t.Run(cmd+"_empty", func(t *testing.T) {
			res, err := Convert(context.Background(), Request{From: cmd, To: "json", Data: []byte("   \n\t\n")})
			if err != nil {
				t.Fatalf("unexpected error on empty %s: %v", cmd, err)
			}
			if !bytes.Equal(res.Data, []byte("[]\n")) {
				t.Fatalf("expected []\\n, got %q", string(res.Data))
			}
		})
	}

	// Entity commands return ExitInvalid error on empty input
	entityCmds := []string{
		"cmd:stat", "cmd:id", "cmd:uname",
	}
	for _, cmd := range entityCmds {
		t.Run(cmd+"_empty", func(t *testing.T) {
			_, err := Convert(context.Background(), Request{From: cmd, To: "json", Data: []byte("")})
			if err == nil {
				t.Fatalf("expected error on empty %s, got nil", cmd)
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != ExitInvalid {
				t.Fatalf("expected ExitInvalid error for empty %s, got: %v", cmd, err)
			}
		})
	}
}

func TestCmdMalformedInputs(t *testing.T) {
	cases := []struct {
		cmd   string
		input string
	}{
		// ps
		{"cmd:ps", "USER COMMAND\nroot /bin/bash"},  // missing PID
		{"cmd:ps", "PID CMD\nnotanumber /bin/bash"}, // non-numeric PID
		{"cmd:ps", "USER PID COMMAND\nroot 123"},    // insufficient columns
		// df
		{"cmd:df", "Filesystem 1K-blocks Used Available\n/dev/sda1 10 5 5"},                          // missing Mounted on
		{"cmd:df", "Size Used Avail Mounted on\n10 5 5 /"},                                           // missing Filesystem
		{"cmd:df", "Filesystem 1K-blocks Used Available Use% Mounted on\n/dev/sda1 10 5 5 badpct /"}, // bad %
		// ls-l
		{"cmd:ls-l", "invalid_perms 1 user group 100 Oct 10 12:00 file"},     // invalid permissions
		{"cmd:ls-l", "-rw-r--r-- badlinks user group 100 Oct 10 12:00 file"}, // invalid links
		{"cmd:ls-l", "-rw-r--r-- 1 user group badsize Oct 10 12:00 file"},    // invalid size
		{"cmd:ls-l", "-rw-r--r-- 1 user"},                                    // truncated
		// du
		{"cmd:du", "notanumber\t/path"},    // invalid blocks
		{"cmd:du", "100pathwithoutspaces"}, // missing separation
		// wc
		{"cmd:wc", "bad tokens here for wc"}, // non-numeric
		// stat
		{"cmd:stat", "This is just random text without headers"},                // missing File:
		{"cmd:stat", "File: a.txt\nSize: bad\nDevice: 1\nAccess: (0644/-rw)\n"}, // malformed size
		// id
		{"cmd:id", "uid=bad(alice) gid=1000(alice)"},     // bad numeric uid
		{"cmd:id", "gid=1000(alice) groups=1000(alice)"}, // missing uid
		{"cmd:id", "uid=1000(alice) groups=1000(alice)"}, // missing gid
		// uname
		{"cmd:uname", ""}, // empty
		// tar-tv
		{"cmd:tar-tv", "short row"},                                           // insufficient columns
		{"cmd:tar-tv", "notamode user/group 1024 2026-10-18 12:00 file"},      // invalid mode
		{"cmd:tar-tv", "-rw-r--r-- user/group badsize 2026-10-18 12:00 file"}, // invalid size
		// ip-route
		{"cmd:ip-route", "default via"},     // via without gateway
		{"cmd:ip-route", "10.0.0.0/24 dev"}, // dev without interface
		{"cmd:ip-route", "default metric"},  // metric without value
		{"cmd:ip-route", "broadcast"},       // type without destination
		// netstat-rn
		{"cmd:netstat-rn", "no header here\ndefault 1.1.1.1 UG en0"},               // missing header
		{"cmd:netstat-rn", "Destination Gateway Flags Netif\ndefault 192.168.1.1"}, // insufficient columns
		{"cmd:netstat-rn", "Destination Gateway Flags Netif\ndefault"},             // truncated row
	}

	for i, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			_, err := Convert(context.Background(), Request{From: tc.cmd, To: "json", Data: []byte(tc.input)})
			if err == nil {
				t.Fatalf("case %d (%s) expected error, got nil", i, tc.cmd)
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != ExitInvalid {
				t.Fatalf("case %d (%s) expected ExitInvalid error, got %v", i, tc.cmd, err)
			}
		})
	}
}
