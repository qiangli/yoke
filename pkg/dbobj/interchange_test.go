package dbobj

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

func TestInterchange_RoundTrips_MemoryAndPersistent(t *testing.T) {
	cases := []struct {
		name       string
		srcEngine  string
		srcMode    dbstore.Mode
		destEngine string
		destMode   dbstore.Mode
	}{
		{"mem_to_mem_sqlite", "sqlite", dbstore.Memory, "sqlite", dbstore.Memory},
		{"mem_to_persistent_sqlite", "sqlite", dbstore.Memory, "sqlite", dbstore.Persistent},
		{"persistent_to_mem_sqlite", "sqlite", dbstore.Persistent, "sqlite", dbstore.Memory},
		{"mem_to_mem_bbolt", "bbolt", dbstore.Memory, "bbolt", dbstore.Memory},
		{"mem_to_persistent_bbolt", "bbolt", dbstore.Memory, "bbolt", dbstore.Persistent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			defer mem.Close()

			var srcTarget dbstore.Target
			if tc.srcMode == dbstore.Memory {
				srcTarget = dbstore.Target{Engine: tc.srcEngine, Mode: dbstore.Memory, Session: mem.ID(), Name: "src"}
			} else {
				srcTarget = dbstore.Target{Engine: tc.srcEngine, Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "src.db")}
			}

			_, err := svc.Create(context.Background(), srcTarget, 5*time.Second)
			if err != nil {
				t.Fatalf("create src: %v", err)
			}

			// Populate source database
			mustRun(t, svc, srcTarget, OpBucketCreate, Input{Bucket: "b1"}, false)
			mustRun(t, svc, srcTarget, OpBucketCreate, Input{Bucket: "b2"}, false)

			mustRun(t, svc, srcTarget, OpPut, Input{
				Bucket:      "b1",
				Key:         "doc1.txt",
				ContentType: "text/plain",
				Metadata:    map[string]string{"env": "test", "id": "1"},
				Value:       &Value{Text: strPtr("content one")},
			}, false)
			mustRun(t, svc, srcTarget, OpPut, Input{
				Bucket:      "b1",
				Key:         "binary.dat",
				ContentType: "application/octet-stream",
				Value:       &Value{Text: strPtr("binary content")},
			}, false)
			mustRun(t, svc, srcTarget, OpPut, Input{
				Bucket:      "b2",
				Key:         "doc2.txt",
				ContentType: "text/plain",
				Value:       &Value{Text: strPtr("content two")},
			}, false)

			// Export to exportDir
			exportDir := filepath.Join(t.TempDir(), "export-out")
			out := mustRun(t, svc, srcTarget, OpExport, Input{Out: exportDir}, false)
			expRes := out.Result.(map[string]any)
			if expRes["buckets"].(int) != 2 || expRes["records"].(int64) != 3 {
				t.Fatalf("unexpected export result: %+v", expRes)
			}

			// Verify manifest exists and is valid
			manBytes, err := os.ReadFile(filepath.Join(exportDir, ManifestName))
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			var m manifest
			if err := json.Unmarshal(manBytes, &m); err != nil {
				t.Fatalf("unmarshal manifest: %v", err)
			}
			if m.SchemaVersion != ExportSchema || m.FormatVersion != FormatVersion || len(m.Files) != 2 {
				t.Fatalf("unexpected manifest contents: %+v", m)
			}

			// Create destination database
			var destTarget dbstore.Target
			if tc.destMode == dbstore.Memory {
				destTarget = dbstore.Target{Engine: tc.destEngine, Mode: dbstore.Memory, Session: mem.ID(), Name: "dest"}
			} else {
				destTarget = dbstore.Target{Engine: tc.destEngine, Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "dest.db")}
			}
			_, err = svc.Create(context.Background(), destTarget, 5*time.Second)
			if err != nil {
				t.Fatalf("create dest: %v", err)
			}

			// Import into destination
			impOut := mustRun(t, svc, destTarget, OpImport, Input{In: exportDir}, false)
			impRes := impOut.Result.(map[string]any)
			if impRes["buckets"].(int) != 2 || impRes["records"].(int64) != 3 {
				t.Fatalf("unexpected import result: %+v", impRes)
			}

			// Verify imported data in destination
			bktOut := mustRun(t, svc, destTarget, OpBucketList, Input{}, false)
			buckets := bktOut.Result.(map[string]any)["buckets"].([]BucketInfo)
			if len(buckets) != 2 {
				t.Fatalf("expected 2 buckets, got %d", len(buckets))
			}

			getOut1 := mustRun(t, svc, destTarget, OpGet, Input{Bucket: "b1", Key: "doc1.txt"}, false)
			obj1 := getOut1.Result.(map[string]any)["object"].(*ObjectInfo)
			val1 := getOut1.Result.(map[string]any)["value"].(Value)
			if obj1.ContentType != "text/plain" || *val1.Text != "content one" || obj1.Metadata["env"] != "test" {
				t.Fatalf("imported object 1 mismatch: %+v, %+v", obj1, val1)
			}

			getOut2 := mustRun(t, svc, destTarget, OpGet, Input{Bucket: "b2", Key: "doc2.txt"}, false)
			val2 := getOut2.Result.(map[string]any)["value"].(Value)
			if *val2.Text != "content two" {
				t.Fatalf("imported object 2 mismatch: %+v", val2)
			}
		})
	}
}

func TestInterchange_ManifestValidation(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	srcTarget := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "src"}
	_, err := svc.Create(context.Background(), srcTarget, 5*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mustRun(t, svc, srcTarget, OpBucketCreate, Input{Bucket: "b"}, false)
	mustRun(t, svc, srcTarget, OpPut, Input{
		Bucket: "b",
		Key:    "k",
		Value:  &Value{Text: strPtr("val")},
	}, false)

	exportDir := filepath.Join(t.TempDir(), "export-val")
	mustRun(t, svc, srcTarget, OpExport, Input{Out: exportDir}, false)

	destTarget := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "dest"}
	_, _ = svc.Create(context.Background(), destTarget, 5*time.Second)

	// 1. Corrupted manifest JSON
	badManifestDir := filepath.Join(t.TempDir(), "bad-man")
	copyDir(t, exportDir, badManifestDir)
	os.WriteFile(filepath.Join(badManifestDir, ManifestName), []byte("invalid json"), 0o644)
	mustFail(t, svc, destTarget, OpImport, Input{In: badManifestDir}, false, dbstore.ErrVersionUnsupported)

	// 2. Unsupported schema version
	badSchemaDir := filepath.Join(t.TempDir(), "bad-schema")
	copyDir(t, exportDir, badSchemaDir)
	editManifest(t, badSchemaDir, func(m *manifest) { m.SchemaVersion = "bad-v9" })
	mustFail(t, svc, destTarget, OpImport, Input{In: badSchemaDir}, false, dbstore.ErrVersionUnsupported)

	// 3. Unsupported format major version (e.g. 2.0)
	badMajorDir := filepath.Join(t.TempDir(), "bad-major")
	copyDir(t, exportDir, badMajorDir)
	editManifest(t, badMajorDir, func(m *manifest) { m.FormatVersion = "2.0" })
	mustFail(t, svc, destTarget, OpImport, Input{In: badMajorDir}, false, dbstore.ErrVersionUnsupported)

	// 4. Corrupted record file (checksum mismatch)
	badRecordDir := filepath.Join(t.TempDir(), "bad-record")
	copyDir(t, exportDir, badRecordDir)
	man := readManifest(t, badRecordDir)
	if len(man.Files) > 0 {
		os.WriteFile(filepath.Join(badRecordDir, man.Files[0].Path), []byte("tampered records\n"), 0o644)
	}
	mustFail(t, svc, destTarget, OpImport, Input{In: badRecordDir}, false, dbstore.ErrIntegrity)

	// 5. Corrupted blob file (checksum mismatch)
	badBlobDir := filepath.Join(t.TempDir(), "bad-blob")
	copyDir(t, exportDir, badBlobDir)
	if len(man.Blobs) > 0 {
		os.WriteFile(filepath.Join(badBlobDir, man.Blobs[0].Path), []byte("tampered blob bytes"), 0o644)
	}
	mustFail(t, svc, destTarget, OpImport, Input{In: badBlobDir}, false, dbstore.ErrIntegrity)
}

func TestInterchange_ConflictPolicies(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	defer mem.Close()

	srcTarget := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "src"}
	_, _ = svc.Create(context.Background(), srcTarget, 5*time.Second)

	mustRun(t, svc, srcTarget, OpBucketCreate, Input{Bucket: "b"}, false)
	mustRun(t, svc, srcTarget, OpPut, Input{
		Bucket: "b",
		Key:    "conflicted",
		Value:  &Value{Text: strPtr("source value")},
	}, false)

	exportDir := filepath.Join(t.TempDir(), "export-conflict")
	mustRun(t, svc, srcTarget, OpExport, Input{Out: exportDir}, false)

	destTarget := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "dest"}
	_, _ = svc.Create(context.Background(), destTarget, 5*time.Second)
	mustRun(t, svc, destTarget, OpBucketCreate, Input{Bucket: "b"}, false)
	mustRun(t, svc, destTarget, OpPut, Input{
		Bucket: "b",
		Key:    "conflicted",
		Value:  &Value{Text: strPtr("dest original value")},
	}, false)

	// 1. Default (error): fails on conflict
	mustFail(t, svc, destTarget, OpImport, Input{In: exportDir}, false, dbstore.ErrConflict)

	// 2. OnConflict: skip preserves existing
	mustRun(t, svc, destTarget, OpImport, Input{In: exportDir, OnConflict: "skip"}, false)
	getOut := mustRun(t, svc, destTarget, OpGet, Input{Bucket: "b", Key: "conflicted"}, false)
	val := getOut.Result.(map[string]any)["value"].(Value)
	if *val.Text != "dest original value" {
		t.Fatalf("expected dest value preserved on skip, got: %s", *val.Text)
	}

	// 3. OnConflict: replace overwrites existing
	mustRun(t, svc, destTarget, OpImport, Input{In: exportDir, OnConflict: "replace"}, false)
	getOut2 := mustRun(t, svc, destTarget, OpGet, Input{Bucket: "b", Key: "conflicted"}, false)
	val2 := getOut2.Result.(map[string]any)["value"].(Value)
	if *val2.Text != "source value" {
		t.Fatalf("expected source value overwritten on replace, got: %s", *val2.Text)
	}

	// 4. Whole --replace requires yes / AccessExclusive
	mustFail(t, svc, destTarget, OpImport, Input{In: exportDir, Replace: true}, false, dbstore.ErrConfirmationRequired)
	mustRun(t, svc, destTarget, OpImport, Input{In: exportDir, Replace: true}, true)
}

func copyDir(t *testing.T, src, dest string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copyDir %s to %s failed: %v", src, dest, err)
	}
}

func readManifest(t *testing.T, dir string) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatalf("read manifest in %s: %v", dir, err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal manifest in %s: %v", dir, err)
	}
	return m
}

func editManifest(t *testing.T, dir string, edit func(*manifest)) {
	t.Helper()
	m := readManifest(t, dir)
	edit(&m)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal edited manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), b, 0o644); err != nil {
		t.Fatalf("write edited manifest: %v", err)
	}
}

func TestImport_BadLaterRecord_LeavesTargetUnchanged(t *testing.T) {
	for _, engine := range []string{"sqlite", "bbolt"} {
		t.Run(engine, func(t *testing.T) {
			svc, mem := newTestService(t, dbstore.LifetimeInvocation)
			tmpDir := t.TempDir()

			// Source database with 2 objects
			srcTarget := dbstore.Target{Engine: engine, Mode: dbstore.Memory, Session: mem.ID(), Name: "src-" + engine}
			_, _ = svc.Create(context.Background(), srcTarget, 5*time.Second)
			mustRun(t, svc, srcTarget, OpBucketCreate, Input{Bucket: "bkt"}, false)
			mustRun(t, svc, srcTarget, OpPut, Input{Bucket: "bkt", Key: "good-1", Value: &Value{Text: strPtr("v1")}}, false)
			mustRun(t, svc, srcTarget, OpPut, Input{Bucket: "bkt", Key: "good-2", Value: &Value{Text: strPtr("v2")}}, false)

			exportDir := filepath.Join(tmpDir, "export-base-"+engine)
			mustRun(t, svc, srcTarget, OpExport, Input{Out: exportDir}, false)

			// Destination target with 1 initial object
			destTarget := dbstore.Target{Engine: engine, Mode: dbstore.Memory, Session: mem.ID(), Name: "dest-" + engine}
			_, _ = svc.Create(context.Background(), destTarget, 5*time.Second)
			mustRun(t, svc, destTarget, OpBucketCreate, Input{Bucket: "bkt"}, false)
			mustRun(t, svc, destTarget, OpPut, Input{Bucket: "bkt", Key: "initial-key", Value: &Value{Text: strPtr("initial")}}, false)

			// Tamper with the export directory: append an invalid record (bad JSON) to the record file
			corruptDir := filepath.Join(tmpDir, "export-corrupt-"+engine)
			copyDir(t, exportDir, corruptDir)

			m := readManifest(t, corruptDir)
			rfPath := filepath.Join(corruptDir, m.Files[0].Path)
			f, err := os.OpenFile(rfPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatalf("open record file: %v", err)
			}
			// Write invalid json record line
			_, _ = f.WriteString("{not valid json\n")
			f.Close()

			// Recompute sha256 of the record file so it passes file-level sha check,
			// forcing failure inside line-by-line record validation
			rfData, _ := os.ReadFile(rfPath)
			h := sha256.Sum256(rfData)
			newSHA := hex.EncodeToString(h[:])
			editManifest(t, corruptDir, func(m *manifest) {
				m.Files[0].SHA256 = newSHA
			})

			// Run import - must fail due to malformed record
			rawImport, _ := json.Marshal(Input{In: corruptDir})
			_, _, err = svc.RunCategory(context.Background(), destTarget, OpImport, rawImport, false, 5*time.Second)
			if err == nil {
				t.Fatalf("expected import to fail on bad record, got nil")
			}
			if !errors.Is(err, dbstore.ErrIntegrity) {
				t.Fatalf("expected ErrIntegrity, got %v", err)
			}

			// Verify destination target is 100% UNCHANGED: only initial-key exists, good-1 was NOT added
			listOut := mustRun(t, svc, destTarget, OpList, Input{Bucket: "bkt"}, false)
			objs := listOut.Result.(map[string]any)["objects"].([]ObjectInfo)
			if len(objs) != 1 || objs[0].Key != "initial-key" {
				t.Fatalf("target was mutated by failed import! objects: %+v", objs)
			}
		})
	}
}

func TestImport_PathTraversalRefused(t *testing.T) {
	svc, mem := newTestService(t, dbstore.LifetimeInvocation)
	tmpDir := t.TempDir()

	srcTarget := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "src"}
	_, _ = svc.Create(context.Background(), srcTarget, 5*time.Second)
	mustRun(t, svc, srcTarget, OpBucketCreate, Input{Bucket: "b"}, false)
	mustRun(t, svc, srcTarget, OpPut, Input{Bucket: "b", Key: "k", Value: &Value{Text: strPtr("v")}}, false)

	exportDir := filepath.Join(tmpDir, "export-trav")
	mustRun(t, svc, srcTarget, OpExport, Input{Out: exportDir}, false)

	destTarget := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "dest"}
	_, _ = svc.Create(context.Background(), destTarget, 5*time.Second)

	// Tamper manifest with a path escaping the import directory
	travDir := filepath.Join(tmpDir, "export-trav-bad")
	copyDir(t, exportDir, travDir)
	editManifest(t, travDir, func(m *manifest) {
		m.Files[0].Path = "../escape.jsonl"
	})

	rawImport, _ := json.Marshal(Input{In: travDir})
	_, _, err := svc.RunCategory(context.Background(), destTarget, OpImport, rawImport, false, 5*time.Second)
	if err == nil {
		t.Fatalf("expected traversal in manifest to be refused, got nil")
	}
	if !errors.Is(err, dbstore.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
