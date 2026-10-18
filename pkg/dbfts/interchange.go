package dbfts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/blevesearch/bleve/v2"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/blevedb"
)

const ManifestName = "manifest.json"
const RecordsName = "documents.jsonl"
const ExportSchema = "bashy-fts-export-v1"
const MaxImportRecords = 100000
const MaxImportBytes = 128 << 20
const MaxManifestBytes = 1 << 20
const MaxRecordBytes = 8 << 20

type manifest struct {
	SchemaVersion string          `json:"schema_version"`
	FormatVersion int             `json:"format_version"`
	Engine        string          `json:"engine"`
	Mapping       json.RawMessage `json:"mapping"`
	Records       string          `json:"records"`
	SHA256        string          `json:"sha256"`
	Count         int             `json:"count"`
}

func allSources(ctx context.Context, d *blevedb.DB) ([]DocumentEntry, error) {
	var out []DocumentEntry
	var total int
	err := eachSource(ctx, d, func(e DocumentEntry) error {
		total += len(e.ID) + len(e.Doc) + 128
		if len(out) >= MaxImportRecords || total > MaxImportBytes {
			return dbstore.ErrLimitExceeded
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func eachSource(ctx context.Context, d *blevedb.DB, fn func(DocumentEntry) error) error {
	idx := d.Index()
	for from := 0; ; {
		if err := ctx.Err(); err != nil {
			return dbstore.FromContext(err)
		}
		req := bleve.NewSearchRequestOptions(bleve.NewMatchAllQuery(), 500, from, false)
		req.SortBy([]string{"_id"})
		res, err := idx.SearchInContext(ctx, req)
		if err != nil {
			return err
		}
		for _, hit := range res.Hits {
			raw, err := idx.GetInternal(sourceKey(hit.ID))
			if err != nil {
				return err
			}
			if raw == nil {
				return fmt.Errorf("%w: source missing for %s", dbstore.ErrIntegrity, hit.ID)
			}
			if err := fn(DocumentEntry{ID: hit.ID, Doc: raw}); err != nil {
				return err
			}
		}
		from += len(res.Hits)
		if from >= int(res.Total) || len(res.Hits) == 0 {
			break
		}
	}
	return nil
}

func runExport(ctx context.Context, d *blevedb.DB, in Input) (dblifecycle.Outcome, error) {
	if in.Out == "" || in.Out == "-" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --out directory required", dbstore.ErrInvalidInput)
	}
	if _, err := os.Stat(in.Out); err == nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export directory", dbstore.ErrExists)
	} else if !os.IsNotExist(err) {
		return dblifecycle.Outcome{}, err
	}
	mapping, err := json.Marshal(d.Index().Mapping())
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(in.Out), ".fts-export-")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer os.RemoveAll(dir)
	f, err := os.OpenFile(filepath.Join(dir, RecordsName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	hash := sha256.New()
	writer := bufio.NewWriter(io.MultiWriter(f, hash))
	count := 0
	var total int
	err = eachSource(ctx, d, func(e DocumentEntry) error {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if len(b)+1 > MaxRecordBytes || count >= MaxImportRecords || total > MaxImportBytes-len(b)-1 {
			return dbstore.ErrLimitExceeded
		}
		if _, err := writer.Write(append(b, '\n')); err != nil {
			return err
		}
		count++
		total += len(b) + 1
		return nil
	})
	if err != nil {
		f.Close()
		return dblifecycle.Outcome{}, err
	}
	if err := writer.Flush(); err != nil {
		f.Close()
		return dblifecycle.Outcome{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return dblifecycle.Outcome{}, err
	}
	if err := f.Close(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	m := manifest{SchemaVersion: ExportSchema, FormatVersion: 1, Engine: "bleve", Mapping: mapping, Records: RecordsName, SHA256: hex.EncodeToString(hash.Sum(nil)), Count: count}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ManifestName), append(b, '\n'), 0600); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := os.Rename(dir, in.Out); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"path": in.Out, "documents": count, "schema_version": ExportSchema}}, nil
}

func readExport(ctx context.Context, path string) (manifest, []DocumentEntry, error) {
	var m manifest
	mf, err := os.Open(filepath.Join(path, ManifestName))
	if err != nil {
		return m, nil, err
	}
	defer mf.Close()
	b, err := io.ReadAll(io.LimitReader(mf, MaxManifestBytes+1))
	if err != nil {
		return m, nil, err
	}
	if len(b) > MaxManifestBytes {
		return m, nil, dbstore.ErrLimitExceeded
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, nil, fmt.Errorf("%w: manifest: %v", dbstore.ErrMalformedInput, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return m, nil, dbstore.ErrMalformedInput
	}
	if m.SchemaVersion != ExportSchema || m.FormatVersion != 1 {
		return m, nil, dbstore.ErrVersionUnsupported
	}
	if m.Engine != "bleve" || m.Records != RecordsName || m.Count < 0 {
		return m, nil, dbstore.ErrMalformedInput
	}
	if m.Count > MaxImportRecords {
		return m, nil, dbstore.ErrLimitExceeded
	}
	f, err := os.Open(filepath.Join(path, RecordsName))
	if err != nil {
		return m, nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return m, nil, err
	} else if st.Size() > MaxImportBytes {
		return m, nil, dbstore.ErrLimitExceeded
	}
	hash := sha256.New()
	r := bufio.NewScanner(io.TeeReader(f, hash))
	r.Buffer(make([]byte, 64<<10), MaxRecordBytes+1)
	entries := make([]DocumentEntry, 0, m.Count)
	seen := map[string]bool{}
	var total int
	for r.Scan() {
		if err := ctx.Err(); err != nil {
			return m, nil, dbstore.FromContext(err)
		}
		line := r.Bytes()
		total += len(line) + 1
		if total > MaxImportBytes {
			return m, nil, dbstore.ErrLimitExceeded
		}
		var e DocumentEntry
		if json.Unmarshal(bytes.TrimSpace(line), &e) != nil || e.ID == "" || seen[e.ID] {
			return m, nil, dbstore.ErrMalformedInput
		}
		if _, err := document(e.Doc); err != nil {
			return m, nil, err
		}
		seen[e.ID] = true
		entries = append(entries, e)
		if len(entries) > m.Count {
			return m, nil, dbstore.ErrMalformedInput
		}
	}
	if err := r.Err(); err != nil {
		return m, nil, fmt.Errorf("%w: record exceeds %d bytes or cannot be scanned: %v", dbstore.ErrLimitExceeded, MaxRecordBytes, err)
	}
	if len(entries) != m.Count || hex.EncodeToString(hash.Sum(nil)) != m.SHA256 {
		return m, nil, dbstore.ErrIntegrity
	}
	return m, entries, nil
}

func runImport(ctx context.Context, d *blevedb.DB, in Input) (dblifecycle.Outcome, error) {
	if in.In == "" || in.In == "-" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --in directory required", dbstore.ErrInvalidInput)
	}
	m, entries, err := readExport(ctx, in.In)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	mapping, err := json.Marshal(d.Index().Mapping())
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var sourceCompact, targetCompact bytes.Buffer
	if json.Compact(&sourceCompact, m.Mapping) != nil || json.Compact(&targetCompact, mapping) != nil || !bytes.Equal(sourceCompact.Bytes(), targetCompact.Bytes()) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: index mapping differs", dbstore.ErrConflict)
	}
	policy := in.OnConflict
	if policy == "" {
		policy = "error"
	}
	if policy != "error" && policy != "skip" && policy != "replace" {
		return dblifecycle.Outcome{}, dbstore.ErrInvalidInput
	}
	idx := d.Index()
	batch := idx.NewBatch()
	count := 0
	if in.Replace {
		existing, err := allSources(ctx, d)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		for _, e := range existing {
			batch.Delete(e.ID)
			batch.DeleteInternal(sourceKey(e.ID))
		}
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return dblifecycle.Outcome{}, dbstore.FromContext(err)
		}
		old, err := idx.GetInternal(sourceKey(e.ID))
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if old != nil && !in.Replace {
			if policy == "error" {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: document %s", dbstore.ErrConflict, e.ID)
			}
			if policy == "skip" {
				continue
			}
		}
		doc, _ := document(e.Doc)
		if err := batch.Index(e.ID, doc); err != nil {
			return dblifecycle.Outcome{}, err
		}
		batch.SetInternal(sourceKey(e.ID), e.Doc)
		count++
	}
	if err := ctx.Err(); err != nil {
		return dblifecycle.Outcome{}, dbstore.FromContext(err)
	}
	if err := bumpGeneration(idx, batch); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := idx.Batch(batch); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"imported": count, "replaced": in.Replace}}, nil
}

func runRebuild(ctx context.Context, d *blevedb.DB, in Input) (dblifecycle.Outcome, error) {
	if len(in.Mapping) != 0 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: changing a live Bleve mapping requires a newly created target", dbstore.ErrUnsupported)
	}
	entries, err := allSources(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	idx := d.Index()
	batch := idx.NewBatch()
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return dblifecycle.Outcome{}, dbstore.FromContext(err)
		}
		doc, _ := document(e.Doc)
		if err := batch.Index(e.ID, doc); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	if err := bumpGeneration(idx, batch); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := idx.Batch(batch); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"rebuilt": len(entries)}}, nil
}
