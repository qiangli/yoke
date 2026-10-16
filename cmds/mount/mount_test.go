// Behavioral provenance: the argv shapes are the ones k8s.io/mount-utils
// (MakeMountArgsSensitiveWithMountFlags, bindMountSensitive,
// detectSafeNotMountedBehavior) and kubelet's emptyDir/subPath code exec;
// the expected mount(2) calls follow the util-linux mount(8)/umount(8)
// manuals (no util-linux source consulted).
package mountcmd

import (
	"bufio"
	"bytes"
	"context"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

type recorder struct {
	calls    []call
	unmounts []string
	err      error
	mounts   []Entry
}

func (r *recorder) Mount(source, target, fstype string, flags uintptr, data string) error {
	r.calls = append(r.calls, call{source, target, fstype, flags, data})
	return r.err
}

func (r *recorder) Unmount(target string, flags int) error {
	r.unmounts = append(r.unmounts, target)
	return r.err
}

func (r *recorder) Mounts() ([]Entry, error) { return r.mounts, r.err }

func fake(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	old := sys
	sys = r
	t.Cleanup(func() { sys = old })
	return r
}

func run(t *testing.T, tl *tool.Tool, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: "/work", Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")}}
	code := tl.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestKubeletMountShapes(t *testing.T) {
	cases := []struct {
		args []string
		want []call
	}{
		// emptyDir medium=Memory / secret / projected volumes.
		{[]string{"-t", "tmpfs", "-o", "size=67108864,noswap", "tmpfs", "/var/lib/kubelet/pods/u/volumes/kubernetes.io~secret/s"},
			[]call{{"tmpfs", "/var/lib/kubelet/pods/u/volumes/kubernetes.io~secret/s", "tmpfs", 0, "size=67108864,noswap"}}},
		{[]string{"-t", "tmpfs", "tmpfs", "/m"}, []call{{"tmpfs", "/m", "tmpfs", 0, ""}}},
		// emptyDir resize.
		{[]string{"-t", "tmpfs", "-o", "remount,size=1024", "tmpfs", "/m"}, []call{{"tmpfs", "/m", "tmpfs", msRemount, "size=1024"}}},
		// hugepages.
		{[]string{"-t", "hugetlbfs", "-o", "pagesize=2Mi", "nodev", "/h"}, []call{{"nodev", "/h", "hugetlbfs", 0, "pagesize=2Mi"}}},
		// subPath: bind, then bind-remount with the source's statfs flags.
		{[]string{"--no-canonicalize", "-o", "bind", "/proc/7/fd/9", "/sub"}, []call{{"/proc/7/fd/9", "/sub", "", msBind, ""}}},
		{[]string{"--no-canonicalize", "-o", "bind,remount,ro,nodev,nosuid,noexec,relatime", "/proc/7/fd/9", "/sub"},
			[]call{{"/proc/7/fd/9", "/sub", "", msRemount | msBind | msRdonly | msNodev | msNosuid | msNoexec | msRelatime, ""}}},
		// A bind with access flags is bind + bind-remount (kernel ignores them on bind).
		{[]string{"-o", "bind,ro", "/a", "/b"}, []call{{"/a", "/b", "", msBind, ""}, {"none", "/b", "", msRemount | msBind | msRdonly, ""}}},
		{[]string{"--rbind", "/a", "/b"}, []call{{"/a", "/b", "", msBind | msRec, ""}}},
		{[]string{"--make-rshared", "/"}, []call{{"none", "/", "", msShared | msRec, ""}}},
		{[]string{"-o", "remount,ro", "/x"}, []call{{"", "/x", "", msRemount | msRdonly, ""}}},
		{[]string{"-t", "tmpfs", "-o", "ro", "-o", "rw,mode=755", "tmpfs", "rel"}, []call{{"tmpfs", "/work/rel", "tmpfs", 0, "mode=755"}}},
	}
	for _, tc := range cases {
		r := fake(t)
		if _, errb, code := run(t, mountCmd, tc.args...); code != 0 {
			t.Errorf("mount %q: code %d: %s", tc.args, code, errb)
			continue
		}
		if !reflect.DeepEqual(r.calls, tc.want) {
			t.Errorf("mount %q:\n got %+v\nwant %+v", tc.args, r.calls, tc.want)
		}
	}
}

func TestMountErrors(t *testing.T) {
	r := fake(t)
	for _, args := range [][]string{
		{"/only-target"}, // no fstab
		{"tmpfs", "/m"},  // no -t, no probing
		{"-o", "x-systemd.automount", "-t", "tmpfs", "tmpfs", "/m"}, // userspace option
		{"--fake", "/a", "/b"},         // unsupported flag
		{"-t", "tmpfs", "a", "b", "c"}, // arity
	} {
		if _, errb, code := run(t, mountCmd, args...); code == 0 || errb == "" {
			t.Errorf("mount %q: want failure, got %d", args, code)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("usage errors must not call mount(2): %+v", r.calls)
	}
	r.err = syscall.EPERM
	if _, errb, code := run(t, mountCmd, "-t", "tmpfs", "tmpfs", "/m"); code != 32 || !strings.Contains(errb, "must be superuser") {
		t.Fatalf("EPERM: %d %q", code, errb)
	}
}

func TestUmount(t *testing.T) {
	r := fake(t)
	if _, _, code := run(t, umountCmd, "/m"); code != 0 || !reflect.DeepEqual(r.unmounts, []string{"/m"}) {
		t.Fatalf("umount: %d %v", code, r.unmounts)
	}
	// mount-utils' detectSafeNotMountedBehavior greps combined output for
	// "not mounted" when umount-ing a fresh temp dir.
	r.err = syscall.EINVAL
	_, errb, code := run(t, umountCmd, "/tmp/kubelet-detect-safe-umount1")
	if code == 0 || !strings.Contains(errb, "not mounted") {
		t.Fatalf("non-mountpoint: %d %q", code, errb)
	}
	r.err = syscall.EBUSY
	if _, errb, code = run(t, umountCmd, "-f", "/m"); code != 32 || !strings.Contains(errb, "target is busy") {
		t.Fatalf("busy: %d %q", code, errb)
	}
	if _, _, code = run(t, umountCmd); code != 2 {
		t.Fatalf("missing target: %d", code)
	}
}

func TestListMounts(t *testing.T) {
	ents, err := parseMounts(bufio.NewScanner(strings.NewReader(
		"tmpfs /run tmpfs rw,nosuid,nodev 0 0\n/dev/sda1 /mnt/with\\040space ext4 ro 0 0\n")))
	if err != nil || len(ents) != 2 || ents[1].Target != "/mnt/with space" {
		t.Fatalf("parseMounts = %+v, %v", ents, err)
	}
	r := fake(t)
	r.mounts = ents
	out, _, code := run(t, mountCmd)
	if code != 0 || out != "tmpfs on /run type tmpfs (rw,nosuid,nodev)\n/dev/sda1 on /mnt/with space type ext4 (ro)\n" {
		t.Fatalf("list: %d %q", code, out)
	}
}
