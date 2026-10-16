// Package mountcmd implements mount(8) and umount(8) for the shapes
// kubelet (k8s.io/mount-utils) and container runtimes exec, as direct
// mount(2)/umount2(2) calls (golang.org/x/sys/unix) — no util-linux:
//
//	mount [--no-canonicalize] -t tmpfs [-o size=N,noswap,…] tmpfs DIR
//	mount [--no-canonicalize] -t tmpfs -o remount,size=N tmpfs DIR
//	mount [--no-canonicalize] -o bind SRC DIR          (also --bind, --rbind)
//	mount [--no-canonicalize] -o bind,remount,ro,nodev,… SRC DIR
//	mount --make-[r]shared|[r]private|[r]slave|[r]unbindable DIR
//	mount                                              (list /proc/self/mounts)
//	umount [-f] [-l] DIR
//
// A bind with access flags is a bind followed by a bind-remount, as
// util-linux does, because the kernel ignores those flags on the bind
// itself. Options mount(8) does not understand pass to the filesystem as
// data (tmpfs size=, mode=, noswap, hugetlbfs pagesize=, …). umount of a
// path that is not a mount point says "not mounted", which mount-utils'
// detectSafeNotMountedBehavior probes for. Linux only: on other systems
// both commands fail with a clear error.
package mountcmd

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/qiangli/coreutils/tool"
)

var mountCmd = &tool.Tool{
	Name:     "mount",
	Synopsis: "Mount a filesystem (tmpfs/bind/remount/propagation subset; Linux, pure Go).",
	Usage: "mount [--no-canonicalize] [-t TYPE] [-o OPTIONS] [SOURCE] TARGET\n" +
		"   or: mount --bind|--rbind SOURCE TARGET\n   or: mount --make-rshared|--make-private|… TARGET\n   or: mount",
}

var umountCmd = &tool.Tool{
	Name:     "umount",
	Synopsis: "Unmount a filesystem (Linux, pure Go).",
	Usage:    "umount [-f] [-l] TARGET...",
}

func init() {
	mountCmd.Run = runMount
	umountCmd.Run = runUmount
	tool.Register(mountCmd)
	tool.Register(umountCmd)
}

// Linux mount(2) flags (pinned against x/sys/unix in sys_linux.go).
const (
	msRdonly      = 0x1
	msNosuid      = 0x2
	msNodev       = 0x4
	msNoexec      = 0x8
	msSynchronous = 0x10
	msRemount     = 0x20
	msDirsync     = 0x80
	msNoatime     = 0x400
	msNodiratime  = 0x800
	msBind        = 0x1000
	msRec         = 0x4000
	msUnbindable  = 0x20000
	msPrivate     = 0x40000
	msSlave       = 0x80000
	msShared      = 0x100000
	msRelatime    = 0x200000
	msStrictatime = 0x1000000
	msLazytime    = 0x2000000

	mntForce  = 0x1
	mntDetach = 0x2
)

// System is the kernel boundary; tests substitute a recorder.
type System interface {
	Mount(source, target, fstype string, flags uintptr, data string) error
	Unmount(target string, flags int) error
	Mounts() ([]Entry, error)
}

// Entry is one /proc/self/mounts line.
type Entry struct{ Source, Target, FSType, Options string }

var sys System = hostSystem{}

// call is one planned mount(2).
type call struct {
	Source, Target, FSType string
	Flags                  uintptr
	Data                   string
}

// flagOpts are the options mount(8) turns into mount(2) flags; "clear"
// options reset them.
var flagOpts = map[string]uintptr{
	"ro": msRdonly, "nosuid": msNosuid, "nodev": msNodev, "noexec": msNoexec,
	"sync": msSynchronous, "dirsync": msDirsync, "noatime": msNoatime,
	"nodiratime": msNodiratime, "relatime": msRelatime, "strictatime": msStrictatime,
	"lazytime": msLazytime,
}

var clearOpts = map[string]uintptr{
	"rw": msRdonly, "suid": msNosuid, "dev": msNodev, "exec": msNoexec, "async": msSynchronous,
	"atime": msNoatime, "diratime": msNodiratime, "norelatime": msRelatime,
	"nostrictatime": msStrictatime, "nolazytime": msLazytime,
}

var propagation = map[string]uintptr{
	"shared": msShared, "rshared": msShared | msRec, "private": msPrivate, "rprivate": msPrivate | msRec,
	"slave": msSlave, "rslave": msSlave | msRec, "unbindable": msUnbindable, "runbindable": msUnbindable | msRec,
}

// accessFlags survive a bind-remount.
const accessFlags = msRdonly | msNosuid | msNodev | msNoexec | msNoatime | msNodiratime | msRelatime | msStrictatime

// plan turns parsed arguments into the mount(2) calls util-linux would make.
func plan(fstype string, opts []string, operands []string) ([]call, error) {
	var flags, prop uintptr
	var data []string
	bind, rbind, remount := false, false, false
	for _, o := range opts {
		switch {
		case o == "" || o == "defaults" || o == "auto" || o == "noauto" || o == "nofail" || o == "_netdev":
		case o == "bind":
			bind = true
		case o == "rbind":
			bind, rbind = true, true
		case o == "remount":
			remount = true
		case flagOpts[o] != 0:
			flags |= flagOpts[o]
		case clearOpts[o] != 0:
			flags &^= clearOpts[o]
		case propagation[o] != 0:
			prop |= propagation[o]
		case strings.HasPrefix(o, "x-") || o == "user" || o == "users" || o == "nouser" || o == "owner" || o == "group":
			return nil, fmt.Errorf("option %q is a mount(8) userspace option; not supported", o)
		default:
			data = append(data, o) // filesystem-specific: size=, mode=, noswap, pagesize=, …
		}
	}

	var source, target string
	switch len(operands) {
	case 1:
		target = operands[0]
		if !remount && prop == 0 {
			return nil, fmt.Errorf("can't find %s in /etc/fstab (no fstab support; give SOURCE and TARGET)", target)
		}
	case 2:
		source, target = operands[0], operands[1]
	default:
		return nil, fmt.Errorf("bad usage")
	}

	var calls []call
	switch {
	case remount:
		f := msRemount | flags
		if bind {
			f |= msBind
		}
		calls = append(calls, call{source, target, fstype, f, strings.Join(data, ",")})
	case bind:
		f := uintptr(msBind)
		if rbind {
			f |= msRec
		}
		calls = append(calls, call{source, target, "", f, ""})
		if flags&accessFlags != 0 {
			calls = append(calls, call{"none", target, "", msRemount | msBind | flags&accessFlags, ""})
		}
	case prop != 0:
	default:
		if fstype == "" {
			return nil, fmt.Errorf("filesystem type required (-t TYPE); no type probing")
		}
		calls = append(calls, call{source, target, fstype, flags, strings.Join(data, ",")})
	}
	// Propagation changes are separate calls, one type each.
	for _, p := range []uintptr{msShared, msPrivate, msSlave, msUnbindable} {
		if prop&p != 0 {
			calls = append(calls, call{"none", target, "", p | prop&msRec, ""})
		}
	}
	return calls, nil
}

func runMount(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(mountCmd.Name)
	fstype := fs.StringP("types", "t", "", "filesystem type")
	options := fs.StringArrayP("options", "o", nil, "comma-separated mount options")
	fs.BoolP("no-canonicalize", "c", false, "don't canonicalize paths (always the case here)")
	bind := fs.BoolP("bind", "B", false, "bind mount (same as -o bind)")
	rbind := fs.BoolP("rbind", "R", false, "recursive bind mount (same as -o rbind)")
	var makes []*bool
	for _, p := range []string{"shared", "rshared", "private", "rprivate", "slave", "rslave", "unbindable", "runbindable"} {
		makes = append(makes, fs.Bool("make-"+p, false, "set "+p+" propagation"))
	}
	operands, code := tool.Parse(rc, mountCmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(args) == 0 {
		return listMounts(rc)
	}
	var opts []string
	for _, o := range *options {
		opts = append(opts, strings.Split(o, ",")...)
	}
	if *bind {
		opts = append(opts, "bind")
	}
	if *rbind {
		opts = append(opts, "rbind")
	}
	for i, p := range []string{"shared", "rshared", "private", "rprivate", "slave", "rslave", "unbindable", "runbindable"} {
		if *makes[i] {
			opts = append(opts, p)
		}
	}
	calls, err := plan(*fstype, opts, operands)
	if err != nil {
		return tool.UsageError(rc, mountCmd, "%v", err)
	}
	for _, c := range calls {
		if err := sys.Mount(c.Source, rc.Path(c.Target), c.FSType, c.Flags, c.Data); err != nil {
			fmt.Fprintf(rc.Err, "mount: %s: %s.\n", c.Target, describe(err, "mount"))
			return 32 // util-linux: mount failure
		}
	}
	return 0
}

func listMounts(rc *tool.RunContext) int {
	ents, err := sys.Mounts()
	if err != nil {
		fmt.Fprintf(rc.Err, "mount: %v\n", err)
		return 32
	}
	for _, e := range ents {
		fmt.Fprintf(rc.Out, "%s on %s type %s (%s)\n", e.Source, e.Target, e.FSType, e.Options)
	}
	return 0
}

func runUmount(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(umountCmd.Name)
	force := fs.BoolP("force", "f", false, "force unmount (unreachable NFS)")
	lazy := fs.BoolP("lazy", "l", false, "detach now, clean up references later")
	operands, code := tool.Parse(rc, umountCmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, umountCmd, "missing TARGET")
	}
	flags := 0
	if *force {
		flags |= mntForce
	}
	if *lazy {
		flags |= mntDetach
	}
	status := 0
	for _, t := range operands {
		if err := sys.Unmount(rc.Path(t), flags); err != nil {
			msg := describe(err, "unmount")
			if errors.Is(err, syscall.EINVAL) {
				msg = "not mounted" // util-linux wording; mount-utils greps for it
			}
			fmt.Fprintf(rc.Err, "umount: %s: %s.\n", t, msg)
			status = 32
		}
	}
	return status
}

func describe(err error, verb string) string {
	switch {
	case errors.Is(err, syscall.EBUSY):
		return "target is busy"
	case errors.Is(err, syscall.EPERM):
		return "must be superuser to " + verb
	}
	return err.Error()
}

func parseMounts(sc *bufio.Scanner) ([]Entry, error) {
	var out []Entry
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		out = append(out, Entry{unescape(f[0]), unescape(f[1]), f[2], f[3]})
	}
	return out, sc.Err()
}

// unescape decodes the octal escapes (\040 for space, …) of /proc/self/mounts.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
