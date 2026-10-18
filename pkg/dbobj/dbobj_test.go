package dbobj

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	_ "github.com/qiangli/yoke/pkg/dbstore/bboltdb"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

func newTestService(t *testing.T, lifetime string) (dblifecycle.Service, *dbstore.MemSpace) {
	t.Helper()
	mem, err := dbstore.NewMemSpace(lifetime)
	if err != nil {
		t.Fatalf("new mem space: %v", err)
	}
	svc := dblifecycle.Service{Mem: mem}
	return svc, mem
}

func mustRun(t *testing.T, svc dblifecycle.Service, target dbstore.Target, op string, in Input, yes bool) dblifecycle.Outcome {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, out, err := svc.RunCategory(ctx, target, op, raw, yes, 5*time.Second)
	if err != nil {
		t.Fatalf("RunCategory %s failed: %v", op, err)
	}
	return out
}

func mustFail(t *testing.T, svc dblifecycle.Service, target dbstore.Target, op string, in Input, yes bool, wantErr error) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = svc.RunCategory(ctx, target, op, raw, yes, 5*time.Second)
	if err == nil {
		t.Fatalf("RunCategory %s succeeded, want error %v", op, wantErr)
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("RunCategory %s returned error %v, want %v", op, err, wantErr)
	}
}

func strPtr(s string) *string {
	return &s
}

func TestBucketLifecycle_MemoryAndPersistent(t *testing.T) {
	engines := []string{"sqlite", "bbolt"}

	for _, eng := range engines {
		t.Run(eng+"_memory", func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			defer mem.Close()

			target := dbstore.Target{
				Engine:  eng,
				Mode:    dbstore.Memory,
				Session: mem.ID(),
				Name:    "mem-obj-test",
			}
			id, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create target %s: %v", target.Engine, err)
			}
			_ = id

			// 1. Create bucket
			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "media"}, false)

			// 2. Duplicate create fails with ErrExists
			mustFail(t, svc, target, OpBucketCreate, Input{Bucket: "media"}, false, dbstore.ErrExists)

			// 3. Invalid bucket name fails with ErrInvalidInput
			mustFail(t, svc, target, OpBucketCreate, Input{Bucket: "bad/name"}, false, dbstore.ErrInvalidInput)
			mustFail(t, svc, target, OpBucketCreate, Input{Bucket: ""}, false, dbstore.ErrInvalidInput)

			// 4. List buckets
			out := mustRun(t, svc, target, OpBucketList, Input{}, false)
			res := out.Result.(map[string]any)
			buckets := res["buckets"].([]BucketInfo)
			if len(buckets) != 1 || buckets[0].Name != "media" {
				t.Fatalf("unexpected buckets list: %+v", buckets)
			}

			// 5. Drop bucket (exclusive, requires yes)
			mustFail(t, svc, target, OpBucketDrop, Input{Bucket: "media"}, false, dbstore.ErrConfirmationRequired)
			mustRun(t, svc, target, OpBucketDrop, Input{Bucket: "media"}, true)

			// 6. Dropping non-existent bucket fails with ErrNotFound
			mustFail(t, svc, target, OpBucketDrop, Input{Bucket: "media"}, true, dbstore.ErrNotFound)
		})

		t.Run(eng+"_persistent", func(t *testing.T) {
			svc, _ := newTestService(t, dbstore.LifetimeInvocation)
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")

			target := dbstore.Target{
				Engine: eng,
				Mode:   dbstore.Persistent,
				Path:   dbPath,
			}
			_, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create persistent: %v", err)
			}

			// Create bucket and add object
			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "archive"}, false)
			mustRun(t, svc, target, OpPut, Input{
				Bucket:      "archive",
				Key:         "doc1.txt",
				ContentType: "text/plain",
				Value:       &Value{Text: strPtr("persistent data")},
			}, false)

			// Verify survival after opening fresh service
			svc2 := dblifecycle.Service{}
			out := mustRun(t, svc2, target, OpGet, Input{Bucket: "archive", Key: "doc1.txt"}, false)
			res := out.Result.(map[string]any)
			obj := res["object"].(*ObjectInfo)
			val := res["value"].(Value)
			if obj.Size != 15 || *val.Text != "persistent data" {
				t.Fatalf("unexpected object: %+v, value: %+v", obj, val)
			}
		})
	}
}

func TestObjectCRUD_MetadataAndPreconditions(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	target := dbstore.Target{
		Engine:  "sqlite",
		Mode:    dbstore.Memory,
		Session: mem.ID(),
		Name:    "crud-test",
	}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "assets"}, false)

	// 1. Put object with custom metadata and text content
	meta := map[string]string{"env": "prod", "author": "alice"}
	out := mustRun(t, svc, target, OpPut, Input{
		Bucket:      "assets",
		Key:         "logo.svg",
		ContentType: "image/svg+xml",
		Metadata:    meta,
		Value:       &Value{Text: strPtr("<svg>logo</svg>")},
	}, false)
	putRes := out.Result.(map[string]any)
	putObj := putRes["object"].(*ObjectInfo)
	if putObj.Revision != 1 || putObj.Size != 15 || putObj.ContentType != "image/svg+xml" {
		t.Fatalf("unexpected put object: %+v", putObj)
	}
	if putObj.Metadata["author"] != "alice" || putObj.Metadata["env"] != "prod" {
		t.Fatalf("metadata not preserved: %+v", putObj.Metadata)
	}

	// 2. Stat object (verifies metadata without reading full blob)
	out = mustRun(t, svc, target, OpStat, Input{Bucket: "assets", Key: "logo.svg"}, false)
	statObj := out.Result.(map[string]any)["object"].(*ObjectInfo)
	if statObj.Revision != 1 || statObj.Size != 15 || statObj.SHA256 != putObj.SHA256 {
		t.Fatalf("stat mismatch: %+v", statObj)
	}

	// 3. Checksum verification on put
	badSHA := "0000000000000000000000000000000000000000000000000000000000000000"
	mustFail(t, svc, target, OpPut, Input{
		Bucket:   "assets",
		Key:      "logo.svg",
		Value:    &Value{Text: strPtr("<svg>logo</svg>")},
		Checksum: badSHA,
	}, false, dbstore.ErrIntegrity)

	// 4. IfAbsent precondition
	mustFail(t, svc, target, OpPut, Input{
		Bucket:   "assets",
		Key:      "logo.svg",
		Value:    &Value{Text: strPtr("<svg>new</svg>")},
		IfAbsent: true,
	}, false, dbstore.ErrConflict)

	// 5. IfRevision precondition mismatch
	mustFail(t, svc, target, OpPut, Input{
		Bucket:     "assets",
		Key:        "logo.svg",
		Value:      &Value{Text: strPtr("<svg>new</svg>")},
		IfRevision: "99",
	}, false, dbstore.ErrConflict)

	// 6. IfRevision precondition matching: successful CAS update
	out = mustRun(t, svc, target, OpPut, Input{
		Bucket:     "assets",
		Key:        "logo.svg",
		Value:      &Value{Text: strPtr("<svg>updated</svg>")},
		IfRevision: "1",
	}, false)
	updatedObj := out.Result.(map[string]any)["object"].(*ObjectInfo)
	if updatedObj.Revision != 2 {
		t.Fatalf("expected revision 2, got %d", updatedObj.Revision)
	}

	// 7. Get object with --out file
	tmpOut := filepath.Join(t.TempDir(), "got-logo.svg")
	mustRun(t, svc, target, OpGet, Input{
		Bucket: "assets",
		Key:    "logo.svg",
		Out:    tmpOut,
	}, false)
	gotBytes, err := os.ReadFile(tmpOut)
	if err != nil || string(gotBytes) != "<svg>updated</svg>" {
		t.Fatalf("unexpected content read from --out: %q, err: %v", string(gotBytes), err)
	}

	// 8. Delete with revision check
	mustFail(t, svc, target, OpDelete, Input{
		Bucket:     "assets",
		Key:        "logo.svg",
		IfRevision: "1",
	}, false, dbstore.ErrConflict)

	mustRun(t, svc, target, OpDelete, Input{
		Bucket:     "assets",
		Key:        "logo.svg",
		IfRevision: "2",
	}, false)

	// Verify object is gone
	mustFail(t, svc, target, OpStat, Input{Bucket: "assets", Key: "logo.svg"}, false, dbstore.ErrNotFound)
}

func TestBinaryPayloads_Base64(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	target := dbstore.Target{
		Engine:  "bbolt",
		Mode:    dbstore.Memory,
		Session: mem.ID(),
		Name:    "bin-test",
	}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "binbkt"}, false)

	// Generate arbitrary random binary bytes
	randData := make([]byte, 1024)
	if _, err := rand.Read(randData); err != nil {
		t.Fatalf("rand: %v", err)
	}
	expectedSHA := hex.EncodeToString(sha256Sum(randData))
	val := Encode(randData)

	mustRun(t, svc, target, OpPut, Input{
		Bucket:      "binbkt",
		Key:         "random.bin",
		ContentType: "application/octet-stream",
		Value:       &val,
		Checksum:    expectedSHA,
	}, false)

	out := mustRun(t, svc, target, OpGet, Input{Bucket: "binbkt", Key: "random.bin"}, false)
	res := out.Result.(map[string]any)
	obj := res["object"].(*ObjectInfo)
	if obj.Size != 1024 || obj.SHA256 != expectedSHA {
		t.Fatalf("binary object size %d or sha %s mismatch", obj.Size, obj.SHA256)
	}
	getVal := res["value"].(Value)
	gotData, err := getVal.Bytes()
	if err != nil {
		t.Fatalf("decode value: %v", err)
	}
	if !bytes.Equal(gotData, randData) {
		t.Fatalf("data corruption in binary round trip")
	}
}

func TestBoundedListing_AndCursorPagination(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	target := dbstore.Target{
		Engine:  "sqlite",
		Mode:    dbstore.Memory,
		Session: mem.ID(),
		Name:    "page-test",
	}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "dataset"}, false)

	// Insert 25 items: item-00 to item-24
	for i := 0; i < 25; i++ {
		k := fmt.Sprintf("item-%02d", i)
		mustRun(t, svc, target, OpPut, Input{
			Bucket: "dataset",
			Key:    k,
			Value:  &Value{Text: strPtr("payload")},
		}, false)
	}

	// 1. First page with limit 10
	out1 := mustRun(t, svc, target, OpList, Input{Bucket: "dataset", Limit: 10}, false)
	pg1 := out1.Page.(page)
	if pg1.Returned != 10 || !pg1.Truncated || pg1.Cursor == nil {
		t.Fatalf("unexpected page 1: %+v", pg1)
	}
	objs1 := out1.Result.(map[string]any)["objects"].([]ObjectInfo)
	if len(objs1) != 10 || objs1[0].Key != "item-00" || objs1[9].Key != "item-09" {
		t.Fatalf("unexpected objects in page 1: %+v", objs1)
	}

	// 2. Second page using cursor from page 1
	cursor1JSON, _ := json.Marshal(pg1.Cursor)
	out2 := mustRun(t, svc, target, OpList, Input{
		Bucket: "dataset",
		Limit:  10,
		Since:  string(cursor1JSON),
	}, false)
	pg2 := out2.Page.(page)
	if pg2.Returned != 10 || !pg2.Truncated || pg2.Cursor == nil {
		t.Fatalf("unexpected page 2: %+v", pg2)
	}
	objs2 := out2.Result.(map[string]any)["objects"].([]ObjectInfo)
	if len(objs2) != 10 || objs2[0].Key != "item-10" || objs2[9].Key != "item-19" {
		t.Fatalf("unexpected objects in page 2: %+v", objs2)
	}

	// 3. Third page
	cursor2JSON, _ := json.Marshal(pg2.Cursor)
	out3 := mustRun(t, svc, target, OpList, Input{
		Bucket: "dataset",
		Limit:  10,
		Since:  string(cursor2JSON),
	}, false)
	pg3 := out3.Page.(page)
	if pg3.Returned != 5 || pg3.Truncated || pg3.Cursor != nil {
		t.Fatalf("unexpected page 3: %+v", pg3)
	}
	objs3 := out3.Result.(map[string]any)["objects"].([]ObjectInfo)
	if len(objs3) != 5 || objs3[0].Key != "item-20" || objs3[4].Key != "item-24" {
		t.Fatalf("unexpected objects in page 3: %+v", objs3)
	}
}

func TestMaxBytesTruncation(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	target := dbstore.Target{
		Engine:  "sqlite",
		Mode:    dbstore.Memory,
		Session: mem.ID(),
		Name:    "max-bytes-test",
	}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "largebkt"}, false)

	// Write 500 KB payload
	largeData := make([]byte, 500*1024)
	val := Encode(largeData)
	mustRun(t, svc, target, OpPut, Input{
		Bucket: "largebkt",
		Key:    "big.dat",
		Value:  &val,
	}, false)

	// Read with MaxBytes = 100 KB: must not inline, returns locator + truncated: true
	out := mustRun(t, svc, target, OpGet, Input{
		Bucket:   "largebkt",
		Key:      "big.dat",
		MaxBytes: 100 * 1024,
	}, false)
	res := out.Result.(map[string]any)
	if res["truncated"] != true || res["locator"] == nil || res["value"] != nil {
		t.Fatalf("expected truncated read with locator, got: %+v", res)
	}
	if len(out.Warnings) == 0 {
		t.Fatalf("expected warning for truncated payload exceeding max_bytes")
	}
}

func TestBackupTransfer_LocalAndRemote(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	target := dbstore.Target{
		Engine:  "sqlite",
		Mode:    dbstore.Memory,
		Session: mem.ID(),
		Name:    "xfer-test",
	}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "mybucket"}, false)
	mustRun(t, svc, target, OpPut, Input{
		Bucket: "mybucket",
		Key:    "f1.txt",
		Value:  &Value{Text: strPtr("file 1 content")},
	}, false)
	mustRun(t, svc, target, OpPut, Input{
		Bucket: "mybucket",
		Key:    "nested/f2.txt",
		Value:  &Value{Text: strPtr("file 2 content")},
	}, false)

	destDir := filepath.Join(t.TempDir(), "backup-dir")

	// 1. Local backup transfer
	out := mustRun(t, svc, target, OpTransfer, Input{
		Bucket:      "mybucket",
		Destination: destDir,
	}, false)
	res := out.Result.(map[string]any)
	if res["objects"].(int) != 2 || res["bytes"].(int64) != 28 {
		t.Fatalf("unexpected transfer result: %+v", res)
	}

	// Verify files exist at destination
	b1, err := os.ReadFile(filepath.Join(destDir, "f1.txt"))
	if err != nil || string(b1) != "file 1 content" {
		t.Fatalf("f1 not transferred properly: %q", string(b1))
	}
	b2, err := os.ReadFile(filepath.Join(destDir, "nested", "f2.txt"))
	if err != nil || string(b2) != "file 2 content" {
		t.Fatalf("f2 not transferred properly: %q", string(b2))
	}

	// 2. Overwriting non-empty destination without --replace fails
	mustFail(t, svc, target, OpTransfer, Input{
		Bucket:      "mybucket",
		Destination: destDir,
	}, false, dbstore.ErrConfirmationRequired)

	// Overwriting with replace succeeds
	mustRun(t, svc, target, OpTransfer, Input{
		Bucket:      "mybucket",
		Destination: destDir,
		Replace:     true,
	}, true)

	// 3. Mock remote transfer
	oldRunner := rcloneRunner
	defer func() { rcloneRunner = oldRunner }()

	var calledArgs []string
	rcloneRunner = func(ctx context.Context, args []string) ([]byte, error) {
		calledArgs = args
		return []byte("transferred 2 files"), nil
	}

	out = mustRun(t, svc, target, OpTransfer, Input{
		Bucket:      "mybucket",
		Destination: "s3:my-remote-bucket",
	}, false)
	if len(calledArgs) < 3 || calledArgs[0] != "copy" || calledArgs[2] != "s3:my-remote-bucket" {
		t.Fatalf("unexpected rclone args: %v", calledArgs)
	}
}

const childEnv = "TEST_DBOBJ_CHILD"

func TestHelperProcess(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) < 5 || args[0] != "put" {
		fmt.Fprintf(os.Stderr, "invalid child args: %v\n", args)
		os.Exit(2)
	}
	dbPath := args[1]
	bucket := args[2]
	key := args[3]
	val := args[4]

	svc := dblifecycle.Service{}
	target := dbstore.Target{
		Engine: "sqlite",
		Mode:   dbstore.Persistent,
		Path:   dbPath,
	}
	raw, err := json.Marshal(Input{
		Bucket: bucket,
		Key:    key,
		Value:  &Value{Text: &val},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal input: %v\n", err)
		os.Exit(1)
	}
	_, _, err = svc.RunCategory(context.Background(), target, OpPut, raw, false, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "put failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestIndependentProcess_SkippedInShort(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process tests MUST skip testing.Short()")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	svc, _ := newTestService(t, dbstore.LifetimeInvocation)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "proc.db")
	target := dbstore.Target{
		Engine: "sqlite",
		Mode:   dbstore.Persistent,
		Path:   dbPath,
	}
	_, err = svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create persistent: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "concbkt"}, false)

	// Launch two independent child processes concurrently against the same target
	var out1Buf, err1Buf bytes.Buffer
	cmd1 := exec.Command(exe, "-test.run=^TestHelperProcess$", "--", "put", dbPath, "concbkt", "k1", "val1")
	cmd1.Env = append(os.Environ(), childEnv+"=1")
	cmd1.Stdout = &out1Buf
	cmd1.Stderr = &err1Buf

	var out2Buf, err2Buf bytes.Buffer
	cmd2 := exec.Command(exe, "-test.run=^TestHelperProcess$", "--", "put", dbPath, "concbkt", "k2", "val2")
	cmd2.Env = append(os.Environ(), childEnv+"=1")
	cmd2.Stdout = &out2Buf
	cmd2.Stderr = &err2Buf

	if err := cmd1.Start(); err != nil {
		t.Fatalf("start child 1: %v", err)
	}
	if err := cmd2.Start(); err != nil {
		t.Fatalf("start child 2: %v", err)
	}

	if err := cmd1.Wait(); err != nil {
		t.Fatalf("child 1 failed: %v, stderr: %s", err, err1Buf.String())
	}
	if err := cmd2.Wait(); err != nil {
		t.Fatalf("child 2 failed: %v, stderr: %s", err, err2Buf.String())
	}

	// Verify both values exist in the target
	out1 := mustRun(t, svc, target, OpGet, Input{Bucket: "concbkt", Key: "k1"}, false)
	res1 := out1.Result.(map[string]any)["value"].(Value)
	if *res1.Text != "val1" {
		t.Fatalf("expected val1, got %v", *res1.Text)
	}

	out2 := mustRun(t, svc, target, OpGet, Input{Bucket: "concbkt", Key: "k2"}, false)
	res2 := out2.Result.(map[string]any)["value"].(Value)
	if *res2.Text != "val2" {
		t.Fatalf("expected val2, got %v", *res2.Text)
	}
}

func TestNoImplicitSchemaOnRead(t *testing.T) {
	svc, _ := newTestService(t, dbstore.LifetimeInvocation)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "schema_test.db")
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: dbPath}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	// 1. Reading buckets on clean db returns empty, creates NO schema
	out := mustRun(t, svc, target, OpBucketList, Input{}, false)
	res := out.Result.(map[string]any)
	bkts := res["buckets"].([]BucketInfo)
	if len(bkts) != 0 {
		t.Fatalf("expected 0 buckets, got %d", len(bkts))
	}

	// Open raw db and check sqlite_schema
	h, err := svc.Open(context.Background(), target, true, 5*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	sqlH, ok := h.(*sqlitedb.DB)
	if !ok {
		t.Fatalf("expected sqlitedb.DB")
	}
	var count int
	err = sqlH.SQL().QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = '_bashy_obj_buckets'").Scan(&count)
	if err != nil {
		t.Fatalf("check sqlite_schema: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 tables created on read, got %d", count)
	}

	// 2. Reading stat or get on non-existent bucket returns not_found, creates NO schema
	rawStat, _ := json.Marshal(Input{Bucket: "nonexistent", Key: "k"})
	_, _, err = svc.RunCategory(context.Background(), target, OpStat, rawStat, false, 5*time.Second)
	if !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	err = sqlH.SQL().QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = '_bashy_obj_buckets'").Scan(&count)
	if err != nil {
		t.Fatalf("check sqlite_schema: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 tables created after stat not_found, got %d", count)
	}

	// 3. Now create a bucket, table should now exist
	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "realbkt"}, false)
	err = sqlH.SQL().QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = '_bashy_obj_buckets'").Scan(&count)
	if err != nil || count == 0 {
		t.Fatalf("expected table created after bucket create, count=%d err=%v", count, err)
	}
}

func TestPathTraversalRefused_Transfer(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Memory, dbstore.Persistent} {
		t.Run(string(mode), func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			tmpDir := t.TempDir()
			target := dbstore.Target{Engine: "sqlite", Mode: mode}
			if mode == dbstore.Memory {
				target.Session = mem.ID()
				target.Name = "traverse"
			} else {
				target.Path = filepath.Join(tmpDir, "traverse.db")
			}
			_, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create db: %v", err)
			}

			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "bkt"}, false)

			// Store permits opaque key with ..
			mustRun(t, svc, target, OpPut, Input{
				Bucket: "bkt",
				Key:    "../escaped.txt",
				Value:  &Value{Text: strPtr("secret content")},
			}, false)

			// Transfer to a subdirectory must refuse path traversal
			destDir := filepath.Join(tmpDir, "dest")
			rawTransfer, _ := json.Marshal(Input{
				Bucket:      "bkt",
				Destination: destDir,
				Replace:     true,
			})
			_, _, err = svc.RunCategory(context.Background(), target, OpTransfer, rawTransfer, true, 5*time.Second)
			if err == nil {
				t.Fatalf("expected path traversal to be refused, got nil error")
			}
			if !errors.Is(err, dbstore.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput for traversal, got %v", err)
			}

			// Ensure file was not created outside destDir
			outsideFile := filepath.Join(tmpDir, "escaped.txt")
			if _, statErr := os.Stat(outsideFile); statErr == nil {
				t.Fatalf("file %s was created outside destination directory!", outsideFile)
			}
		})
	}
}

func TestTransfer_TruncatedRefused(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	tmpDir := t.TempDir()
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "trunc"}
	_, err := svc.Create(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "bkt"}, false)
	mustRun(t, svc, target, OpPut, Input{Bucket: "bkt", Key: "k1", Value: &Value{Text: strPtr("v1")}}, false)
	mustRun(t, svc, target, OpPut, Input{Bucket: "bkt", Key: "k2", Value: &Value{Text: strPtr("v2")}}, false)

	// Attempt transfer with Limit 1, which causes truncated == true
	destDir := filepath.Join(tmpDir, "dest")
	rawTransfer, _ := json.Marshal(Input{
		Bucket:      "bkt",
		Destination: destDir,
		Limit:       1,
		Replace:     true,
	})
	_, _, err = svc.RunCategory(context.Background(), target, OpTransfer, rawTransfer, true, 5*time.Second)
	if err == nil {
		t.Fatalf("expected error on truncated transfer, got nil")
	}
	if !errors.Is(err, dbstore.ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}

	// Attempt export with Limit 1, which causes truncated == true
	outDir := filepath.Join(tmpDir, "export_out")
	rawExport, _ := json.Marshal(Input{
		Out:   outDir,
		Limit: 1,
	})
	_, _, err = svc.RunCategory(context.Background(), target, OpExport, rawExport, false, 5*time.Second)
	if err == nil {
		t.Fatalf("expected error on truncated export, got nil")
	}
	if !errors.Is(err, dbstore.ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}
}

func TestTransfer_SymlinkEscapeRefused(t *testing.T) {
	for _, eng := range []string{"sqlite", "bbolt"} {
		t.Run(eng, func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			target := dbstore.Target{Engine: eng, Mode: dbstore.Memory, Session: mem.ID(), Name: "symescape"}
			_, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create db: %v", err)
			}

			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "symbkt"}, false)
			mustRun(t, svc, target, OpPut, Input{
				Bucket: "symbkt",
				Key:    "sub/escaped.txt",
				Value:  &Value{Text: strPtr("secret content")},
			}, false)

			// Setup destination with a pre-existing subdirectory that is a symlink pointing outside
			tmpDir := t.TempDir()
			destDir := filepath.Join(tmpDir, "dest")
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				t.Fatalf("mkdir dest: %v", err)
			}
			outsideDir := filepath.Join(tmpDir, "outside")
			if err := os.MkdirAll(outsideDir, 0o755); err != nil {
				t.Fatalf("mkdir outside: %v", err)
			}

			// Pre-existing subdirectory symlink pointing outside destDir
			if err := os.Symlink(outsideDir, filepath.Join(destDir, "sub")); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			// Transfer must reject the symlink escape before writing
			rawTransfer, _ := json.Marshal(Input{
				Bucket:      "symbkt",
				Destination: destDir,
				Replace:     true,
			})
			_, _, err = svc.RunCategory(context.Background(), target, OpTransfer, rawTransfer, true, 5*time.Second)
			if err == nil {
				t.Fatalf("expected transfer with symlink escape to fail, got nil")
			}
			if !errors.Is(err, dbstore.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}

			// Ensure outside file was never written
			escapedFile := filepath.Join(outsideDir, "escaped.txt")
			if _, statErr := os.Stat(escapedFile); statErr == nil {
				t.Fatalf("escaped file was written outside destination directory: %s", escapedFile)
			}
		})
	}
}

func TestTransfer_NormalizedKeyCollisionRefused(t *testing.T) {
	for _, eng := range []string{"sqlite", "bbolt"} {
		t.Run(eng, func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			target := dbstore.Target{Engine: eng, Mode: dbstore.Memory, Session: mem.ID(), Name: "collision"}
			_, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create db: %v", err)
			}

			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "colbkt"}, false)
			// Store permits distinct keys a/b and a//b
			mustRun(t, svc, target, OpPut, Input{
				Bucket: "colbkt",
				Key:    "a/b",
				Value:  &Value{Text: strPtr("content 1")},
			}, false)
			mustRun(t, svc, target, OpPut, Input{
				Bucket: "colbkt",
				Key:    "a//b",
				Value:  &Value{Text: strPtr("content 2")},
			}, false)

			// Transfer must detect collision and refuse to overwrite
			tmpDir := t.TempDir()
			destDir := filepath.Join(tmpDir, "dest")
			rawTransfer, _ := json.Marshal(Input{
				Bucket:      "colbkt",
				Destination: destDir,
				Replace:     true,
			})
			_, _, err = svc.RunCategory(context.Background(), target, OpTransfer, rawTransfer, true, 5*time.Second)
			if err == nil {
				t.Fatalf("expected normalized key collision error, got nil")
			}
			if !errors.Is(err, dbstore.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}

			// Ensure no files were written
			ents, _ := os.ReadDir(destDir)
			if len(ents) != 0 {
				t.Fatalf("expected 0 files written after collision refusal, got %d", len(ents))
			}
		})
	}
}

func TestTransfer_NoncanonicalKeySuccess(t *testing.T) {
	for _, eng := range []string{"sqlite", "bbolt"} {
		t.Run(eng, func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			target := dbstore.Target{Engine: eng, Mode: dbstore.Memory, Session: mem.ID(), Name: "noncanonical"}
			_, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create db: %v", err)
			}

			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "noncanbkt"}, false)
			// Store a single object with noncanonical key (e.g. "a//b")
			mustRun(t, svc, target, OpPut, Input{
				Bucket: "noncanbkt",
				Key:    "a//b",
				Value:  &Value{Text: strPtr("noncanonical payload")},
			}, false)

			// Transfer to a local directory should succeed and write to normalized path a/b
			tmpDir := t.TempDir()
			destDir := filepath.Join(tmpDir, "dest")
			out := mustRun(t, svc, target, OpTransfer, Input{
				Bucket:      "noncanbkt",
				Destination: destDir,
				Replace:     true,
			}, true)

			res, ok := out.Result.(map[string]any)
			if !ok || res["objects"] != 1 {
				t.Fatalf("expected 1 transferred object, got %+v", out.Result)
			}

			writtenFile := filepath.Join(destDir, "a", "b")
			data, err := os.ReadFile(writtenFile)
			if err != nil {
				t.Fatalf("read written normalized file %s: %v", writtenFile, err)
			}
			if string(data) != "noncanonical payload" {
				t.Fatalf("unexpected content: got %q, want %q", string(data), "noncanonical payload")
			}
		})
	}
}

func TestKV_ImportAtomic_MetadataPreserved(t *testing.T) {
	engines := []string{"sqlite", "bbolt"}
	for _, eng := range engines {
		t.Run(eng, func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			target := dbstore.Target{Engine: eng, Mode: dbstore.Memory, Session: mem.ID(), Name: "metapres"}
			_, err := svc.Create(context.Background(), target, 5*time.Second)
			if err != nil {
				t.Fatalf("create db: %v", err)
			}

			// 1. Create source bucket and export
			mustRun(t, svc, target, OpBucketCreate, Input{Bucket: "src_bkt"}, false)
			mustRun(t, svc, target, OpPut, Input{
				Bucket: "src_bkt",
				Key:    "data.txt",
				Value:  &Value{Text: strPtr("hello world")},
			}, false)

			tmpDir := t.TempDir()
			exportDir := filepath.Join(tmpDir, "export")
			mustRun(t, svc, target, OpExport, Input{Out: exportDir}, false)

			// 2. Setup separate destination database with an existing bucket
			dstTarget := dbstore.Target{Engine: eng, Mode: dbstore.Memory, Session: mem.ID(), Name: "metapres_dst"}
			_, err = svc.Create(context.Background(), dstTarget, 5*time.Second)
			if err != nil {
				t.Fatalf("create dst db: %v", err)
			}
			mustRun(t, svc, dstTarget, OpBucketCreate, Input{Bucket: "existing_bkt"}, false)

			out := mustRun(t, svc, dstTarget, OpBucketList, Input{}, false)
			res := out.Result.(map[string]any)["buckets"].([]BucketInfo)
			var originalCreated string
			for _, b := range res {
				if b.Name == "existing_bkt" {
					originalCreated = b.CreatedAt
					break
				}
			}
			if originalCreated == "" {
				t.Fatalf("bucket existing_bkt not found in catalog")
			}

			// 3. Modify manifest in exportDir so that it includes existing_bkt with a different CreatedAt
			manifestPath := filepath.Join(exportDir, ManifestName)
			mBytes, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			var m manifest
			if err := json.Unmarshal(mBytes, &m); err != nil {
				t.Fatalf("unmarshal manifest: %v", err)
			}
			m.SchemaDef.Buckets = append(m.SchemaDef.Buckets, bucketDef{
				Name:      "existing_bkt",
				CreatedAt: "1999-01-01T00:00:00Z",
			})
			newMBytes, _ := json.MarshalIndent(m, "", "  ")
			if err := os.WriteFile(manifestPath, newMBytes, 0o644); err != nil {
				t.Fatalf("write modified manifest: %v", err)
			}

			// 4. Import without replace into dstTarget - existing_bkt CreatedAt must NOT be overwritten
			rawImport, _ := json.Marshal(Input{
				In:      exportDir,
				Replace: false,
			})
			_, _, err = svc.RunCategory(context.Background(), dstTarget, OpImport, rawImport, false, 5*time.Second)
			if err != nil {
				t.Fatalf("import failed: %v", err)
			}

			// 5. Verify existing_bkt CreatedAt was preserved in dstTarget
			outAfter := mustRun(t, svc, dstTarget, OpBucketList, Input{}, false)
			resAfter := outAfter.Result.(map[string]any)["buckets"].([]BucketInfo)
			var afterCreated string
			for _, b := range resAfter {
				if b.Name == "existing_bkt" {
					afterCreated = b.CreatedAt
					break
				}
			}
			if afterCreated != originalCreated {
				t.Fatalf("expected existing_bkt CreatedAt %q to be preserved, but got %q", originalCreated, afterCreated)
			}
		})
	}
}
