//go:build windows

// Live S515.2 client: uses the real bashy loom, outpost router and SeaweedFS.
// Run on the scratch Windows peer with: bashy go run acceptance_windows.go
package main

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var failures []string
var stack func() func()
var base string
var storeEndpoint = "127.0.0.1:31890"
var client = &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func check(ok bool, format string, a ...any) {
	if !ok {
		panic(fmt.Sprintf(format, a...))
	}
}
func request(method, target string, body []byte, headers map[string]string) (*http.Response, []byte) {
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	must(err)
	if strings.HasPrefix(target, base) {
		req.SetBasicAuth("admin", "admin")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	must(err)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(err)
	return resp, data
}
func jsonreq(method, target string, v any) (*http.Response, []byte) {
	b, err := json.Marshal(v)
	must(err)
	ct := "application/json"
	if strings.HasSuffix(target, "/info/lfs/objects/batch") {
		ct = "application/vnd.git-lfs+json"
	}
	return request(method, target, b, map[string]string{"Content-Type": ct, "Accept": ct})
}
func good(r *http.Response, b []byte) {
	check(r.StatusCode >= 200 && r.StatusCode < 300, "%s: %s", r.Status, string(b))
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func absolute(loc string) string {
	u, err := url.Parse(loc)
	must(err)
	b, err := url.Parse(base)
	must(err)
	return b.ResolveReference(u).String()
}

type tcpRow struct{ State, LocalAddr, LocalPort, RemoteAddr, RemotePort, PID uint32 }

var iphlp = syscall.NewLazyDLL("iphlpapi.dll")

func rows() []tcpRow {
	p := iphlp.NewProc("GetExtendedTcpTable")
	var size uint32
	p.Call(0, uintptr(unsafe.Pointer(&size)), 0, 2, 5, 0)
	buf := make([]byte, size)
	rc, _, _ := p.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 2, 5, 0)
	check(rc == 0, "GetExtendedTcpTable=%d", rc)
	n := int(binary.LittleEndian.Uint32(buf))
	out := make([]tcpRow, n)
	for i := range out {
		out[i] = *(*tcpRow)(unsafe.Pointer(&buf[4+i*24]))
	}
	return out
}
func port(p uint32) int { return int((p&255)<<8 | (p>>8)&255) }
func loomPID(p int) uint32 {
	for _, r := range rows() {
		if port(r.LocalPort) == p && r.State == 2 {
			return r.PID
		}
	}
	panic("loom listener PID not found")
}

type counters struct {
	pid     uint32
	tracked map[[4]uint32]tcpRow
}

func key(r tcpRow) [4]uint32 { return [4]uint32{r.LocalAddr, r.LocalPort, r.RemoteAddr, r.RemotePort} }
func (c *counters) enable() {
	for _, r := range rows() {
		if r.PID != c.pid || r.State != 5 {
			continue
		}
		if _, ok := c.tracked[key(r)]; ok {
			continue
		}
		on := byte(1)
		rc, _, _ := iphlp.NewProc("SetPerTcpConnectionEStats").Call(uintptr(unsafe.Pointer(&r)), 1, uintptr(unsafe.Pointer(&on)), 0, 1, 0)
		check(rc == 0, "SetPerTcpConnectionEStats PID=%d port=%d rc=%d", c.pid, port(r.LocalPort), rc)
		c.tracked[key(r)] = r
	}
	check(len(c.tracked) > 0, "no live loom TCP connections to count")
}
func (c *counters) tx() uint64 {
	var total uint64
	for _, r := range c.tracked {
		var rw byte
		// Windows TCP_ESTATS_DATA_ROD_v0 is 96 bytes on amd64.
		var rod [12]uint64
		rc, _, _ := iphlp.NewProc("GetPerTcpConnectionEStats").Call(uintptr(unsafe.Pointer(&r)), 1, uintptr(unsafe.Pointer(&rw)), 0, 1, 0, 0, 0, uintptr(unsafe.Pointer(&rod)), 0, unsafe.Sizeof(rod))
		check(rc == 0 && rw == 1, "GetPerTcpConnectionEStats rc=%d enabled=%d", rc, rw)
		total += rod[0]
	}
	return total
}
func download(label, target string, want []byte, c *counters) {
	c.enable()
	requestBefore := c.tx()
	r, b := request("GET", target, nil, nil)
	requestAfter := c.tx()
	if r.StatusCode == 200 && os.Getenv("S515_DIAGNOSTIC_ENDPOINT") != "" {
		check(bytes.Equal(b, want), "%s proxied bytes mismatch", label)
		fmt.Printf("%s PROXIED status=200 bytes=%d sha256=%s loom_pid=%d tcp_data_tx_before=%d after=%d delta=%d\n", label, len(b), digest(b), c.pid, requestBefore, requestAfter, requestAfter-requestBefore)
		verifyStore(label, want)
		failures = append(failures, label+" returned 200 with payload through loom")
		return
	}
	check(r.StatusCode >= 300 && r.StatusCode < 400, "%s expected redirect: %s bytes=%d loom_tx_before=%d after=%d delta=%d", label, r.Status, len(b), requestBefore, requestAfter, requestAfter-requestBefore)
	loc := r.Header.Get("Location")
	fmt.Printf("%s redirect=%d\n", label, r.StatusCode)
	directDownload(label, loc, want, c)
}
func directDownload(label, loc string, want []byte, c *counters) {
	u, err := url.Parse(loc)
	must(err)
	check(u.Host == storeEndpoint, "unexpected store name %s", u.Host)
	check(strings.HasPrefix(u.Path, "/s3-s515self/"), "unexpected bucket path %s", u.Path)
	check(u.Query().Get("X-Amz-Signature") != "", "missing presigned signature")
	fmt.Printf("%s store=%s path=%s\n", label, u.Host, u.Path)
	c.enable()
	before := c.tx()
	r, b := request("GET", loc, nil, nil)
	good(r, b)
	after := c.tx()
	check(bytes.Equal(b, want), "%s bytes mismatch", label)
	fmt.Printf("%s bytes=%d sha256=%s loom_pid=%d tcp_data_tx_before=%d after=%d delta=%d connections=%d\n", label, len(b), digest(b), c.pid, before, after, after-before, len(c.tracked))
	check(before == after, "%s loom transmitted bytes during direct store fetch", label)
	direct := "http://127.0.0.1:8333" + u.RequestURI()
	// Dial SeaweedFS directly while retaining the signed Host header.
	r, b = request("GET", direct, nil, map[string]string{"Host": u.Host})
	good(r, b)
	check(bytes.Equal(b, want), "%s direct SeaweedFS bytes mismatch", label)
	fmt.Printf("%s direct_seaweedfs_sha256=%s MATCH\n", label, digest(b))
}
func verifyStore(label string, want []byte) {
	r, b := request("GET", "http://127.0.0.1:8333/loom?list-type=2", nil, nil)
	good(r, b)
	var listing struct {
		Contents []struct {
			Key  string
			Size int64
		}
	}
	must(xml.Unmarshal(b, &listing))
	for _, object := range listing.Contents {
		if object.Size != int64(len(want)) {
			continue
		}
		r, b = request("GET", "http://127.0.0.1:8333/loom/"+object.Key, nil, nil)
		good(r, b)
		if bytes.Equal(b, want) {
			fmt.Printf("%s direct_seaweedfs_sha256=%s MATCH key=%s\n", label, digest(b), object.Key)
			return
		}
	}
	panic(label + " matching SeaweedFS object not found")
}
func run() {
	if stack != nil {
		cleanup := stack()
		defer cleanup()
	}
	root, err := os.Getwd()
	must(err)
	if ep := os.Getenv("S515_DIAGNOSTIC_ENDPOINT"); ep != "" {
		storeEndpoint = ep
		fmt.Println("DIAGNOSTIC ONLY: bypassing stable name with", ep)
	}
	// Reserve a free port for loom; every fixed storage port is task-assigned.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	base = "http://127.0.0.1:" + strconv.Itoa(p)
	log, err := os.Create("loom.log")
	must(err)
	defer log.Close()
	dataDir := filepath.Join(root, "loom-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	must(os.MkdirAll(dataDir, 0700))
	secret := make([]byte, 32)
	_, err = rand.Read(secret)
	must(err)
	ini := fmt.Sprintf("[server]\nLFS_START_SERVER = true\n[database]\nDB_TYPE = sqlite3\nPATH = %s\n[repository]\nROOT = %s\n[security]\nINSTALL_LOCK = true\nSECRET_KEY = %s\n[log]\nMODE = console\n", filepath.ToSlash(filepath.Join(dataDir, "gitea.db")), filepath.ToSlash(filepath.Join(dataDir, "repositories")), hex.EncodeToString(secret))
	if os.Getenv("S515_DIAGNOSTIC_ANONYMOUS") == "1" {
		ini += "\n[storage]\nMINIO_IAM_ENDPOINT = http://127.0.0.1:1\n"
		fmt.Println("DIAGNOSTIC ONLY: anonymous S3 credentials")
	}
	must(os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(ini), 0600))
	cmd := exec.Command(filepath.Join(root, "bashy-s515-fixed.exe"), "loom", "serve", "--data", dataDir, "--port", strconv.Itoa(p))
	cmd.Env = append(os.Environ(), "LOOM_S3_PEER=s515self", "LOOM_S3_ACCESS_KEY=s515-proof", "LOOM_S3_SECRET_KEY=s515-proof-secret")
	if storeEndpoint != "127.0.0.1:31890" {
		cmd.Env = append(cmd.Env, "LOOM_S3_ENDPOINT="+storeEndpoint)
	}
	if os.Getenv("S515_DIAGNOSTIC_ANONYMOUS") == "1" {
		cmd.Env = append(cmd.Env, "LOOM_S3_ACCESS_KEY=", "LOOM_S3_SECRET_KEY=", "HOME="+dataDir, "USERPROFILE="+dataDir)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	must(cmd.Start())
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var pid uint32
	defer func() {
		if pid != 0 {
			h, e := syscall.OpenProcess(1, false, pid)
			if e == nil {
				syscall.TerminateProcess(h, 0)
				syscall.CloseHandle(h)
			}
		}
	}()
	ready := false
	for i := 0; i < 180; i++ {
		select {
		case err := <-done:
			panic(fmt.Sprintf("loom exited: %v; inspect loom.log", err))
		default:
		}
		req, _ := http.NewRequest("GET", base+"/api/v1/user", nil)
		req.SetBasicAuth("admin", "admin")
		r, e := client.Do(req)
		if e == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(time.Second)
	}
	check(ready, "loom did not become ready; inspect loom.log")
	pid = loomPID(p)
	fmt.Printf("loom ready pid=%d port=%d\n", pid, p)
	if os.Getenv("S515_DIAGNOSTIC_ENDPOINT") == "" {
		for _, endpoint := range []string{"127.0.0.1:8333", storeEndpoint} {
			r, _ := request("PUT", "http://"+endpoint+"/s3-s515self/unsigned-proof", []byte("must reject"), nil)
			check(r.StatusCode == http.StatusForbidden, "unsigned write to %s returned %d", endpoint, r.StatusCode)
			fmt.Printf("unsigned_write endpoint=%s status=%d REJECTED\n", endpoint, r.StatusCode)
		}
	}
	c := &counters{pid: pid, tracked: map[[4]uint32]tcpRow{}}
	c.enable()
	// Positive control: ensure this is a functioning process-specific TX counter.
	before := c.tx()
	r, b := request("GET", base+"/api/v1/version", nil, nil)
	good(r, b)
	after := c.tx()
	check(after > before, "TCP counter positive control stayed flat")
	fmt.Printf("counter_positive_control before=%d after=%d delta=%d\n", before, after, after-before)

	// OCI Distribution API: push config, layer and manifest; pull manifest and blobs.
	payload := make([]byte, 8<<20)
	_, err = rand.Read(payload)
	must(err)
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	must(tw.WriteHeader(&tar.Header{Name: "proof.bin", Mode: 0644, Size: int64(len(payload))}))
	_, err = tw.Write(payload)
	must(err)
	must(tw.Close())
	payload = layer.Bytes()
	config, err := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + digest(payload)}}})
	must(err)
	blobs := [][]byte{config, payload}
	for _, blob := range blobs {
		r, b = request("POST", base+"/v2/admin/s515-image/blobs/uploads/", nil, nil)
		good(r, b)
		upload := absolute(r.Header.Get("Location"))
		sep := "?"
		if strings.Contains(upload, "?") {
			sep = "&"
		}
		r, b = request("PUT", upload+sep+"digest=sha256:"+digest(blob), blob, map[string]string{"Content-Type": "application/octet-stream"})
		good(r, b)
	}
	manifest := map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + digest(config), "size": len(config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:" + digest(payload), "size": len(payload)}}}
	mb, err := json.Marshal(manifest)
	must(err)
	r, b = request("PUT", base+"/v2/admin/s515-image/manifests/proof", mb, map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	good(r, b)
	fmt.Printf("OCI push manifest=sha256:%s layer=sha256:%s\n", digest(mb), digest(payload))
	download("OCI manifest", base+"/v2/admin/s515-image/manifests/proof", mb, c)
	download("OCI layer", base+"/v2/admin/s515-image/blobs/sha256:"+digest(payload), payload, c)
	download("OCI config", base+"/v2/admin/s515-image/blobs/sha256:"+digest(config), config, c)

	r, b = jsonreq("POST", base+"/api/v1/user/repos", map[string]any{"name": "s515-lfs", "private": true})
	good(r, b)
	lfs := make([]byte, 4<<20)
	_, err = rand.Read(lfs)
	must(err)
	oid := digest(lfs)
	batch := base + "/admin/s515-lfs.git/info/lfs/objects/batch"
	for _, op := range []string{"upload", "download"} {
		r, b = jsonreq("POST", batch, map[string]any{"operation": op, "transfers": []string{"basic"}, "objects": []any{map[string]any{"oid": oid, "size": len(lfs)}}})
		good(r, b)
		var v struct {
			Objects []struct {
				Actions map[string]struct {
					Href   string
					Header map[string]string
				}
				Error any
			}
		}
		must(json.Unmarshal(b, &v))
		check(len(v.Objects) == 1, "bad LFS batch: %s", b)
		action, ok := v.Objects[0].Actions[op]
		check(ok, "missing LFS %s action: %s", op, b)
		if op == "upload" {
			r, b = request("PUT", action.Href, lfs, action.Header)
			good(r, b)
			fmt.Printf("LFS upload oid=%s bytes=%d\n", oid, len(lfs))
		} else {
			// A standard LFS client follows the batch action, which Gitea
			// supplies as a presigned store URL when SERVE_DIRECT is enabled.
			directDownload("LFS batch action", action.Href, lfs, c)
			// Also retain the requested strict HTTP-redirect gate. The pinned
			// Gitea object endpoint currently proxies despite SERVE_DIRECT.
			download("LFS", base+"/admin/s515-lfs.git/info/lfs/objects/"+oid, lfs, c)
		}
	}
	check(len(failures) == 0, "acceptance failures: %s", strings.Join(failures, "; "))
	if os.Getenv("S515_DIAGNOSTIC_ENDPOINT") != "" {
		fmt.Println("S515.2 DIAGNOSTIC PASS ONLY; stable-name acceptance remains blocked")
	} else {
		fmt.Println("S515.2 LIVE ACCEPTANCE PASS (single-host; keyed loopback S3; bucket route)")
	}
}
func main() {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", p)
			os.Exit(1)
		}
	}()
	run()
}
