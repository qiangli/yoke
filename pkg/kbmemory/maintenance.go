// Memory maintenance, Wave3 of story b9d919218f1a.
//
// Maintain is the one explicit maintenance pipeline for a memory collection.
// Nothing here forgets, expires or deletes by itself and nothing consolidates
// with a model: candidates is a read-only deterministic report (expired,
// stale, duplicate), and every change is a separate call that names one
// record and its revision.
//
// It adds no lifecycle semantics. supersede is the capture verb unchanged
// (pkg/kblife interval rule, one atomic batch); expire closes a valid
// record's interval at one instant inside execution (kblife.Validity.Check is
// the inversion rule); delete is a revision-guarded dbdoc rm. Export/import
// and backup/restore are the accepted dbdoc and dblifecycle operations, so
// record IDs, revisions-as-stored text and provenance travel unchanged.
//
// The full-text index is derived. A mutation (and import/restore) may name an
// --index; after the source commit the explicit SyncIndex pipeline runs. If
// that sync fails the mutation has still committed: the result says so and
// names the recovery (rerun index-sync), and retrieval keeps dropping hits
// the source no longer holds until the index converges.
package kbmemory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/kblife"
)

const (
	// MaintenanceSchema versions successful maintenance envelopes. Errors
	// reuse ErrorSchema unchanged.
	MaintenanceSchema = "bashy-kb-memory-maintenance-v1"
	// OpExpire and OpDelete are the category operations maintenance adds on
	// the shared admission path.
	OpExpire = "memory.expire"
	OpDelete = "memory.delete"
)

// MaintainVerbs, in the order `kb memory maintain` documents them.
var MaintainVerbs = []string{"candidates", "supersede", "expire", "delete", "export", "import", "backup", "restore"}

// Candidate reasons.
const (
	ReasonExpired   = "expired"
	ReasonStale     = "stale"
	ReasonDuplicate = "duplicate"
)

func init() {
	register(OpExpire, runExpire)
	register(OpDelete, runDelete)
	registerExport()
}

// OpExport is the memory-only export: the memory-only check and the export
// run on ONE admitted read handle, so a collection created in between cannot
// leave through kb memory.
const OpExport = "memory.export"

func registerExport() {
	dblifecycle.RegisterCategoryOp(OpExport, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			if _, err := dbdoc.DecodeInput(raw); err != nil {
				return 0, err
			}
			return dblifecycle.AccessRead, nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
			return dbdoc.Spec(OpExport, mode, a)
		},
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			if a != dblifecycle.AccessRead {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: %s admitted with the wrong access", dbstore.ErrInvalidInput, OpExport)
			}
			if err := memoryOnlyOn(ctx, h); err != nil {
				return dblifecycle.Outcome{}, err
			}
			op, acc, ok, err := dblifecycle.LookupCategoryOp(dbdoc.OpExport, raw)
			if !ok || err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: db operation %s", dbstore.ErrUnsupported, dbdoc.OpExport)
			}
			return op.Run(ctx, h, raw, acc)
		},
	})
}

// outcomeOn is onHandle that keeps the whole Outcome, pagination included.
func outcomeOn(ctx context.Context, h dbstore.Handle, name string, in dbdoc.Input) (dblifecycle.Outcome, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	op, a, ok, err := dblifecycle.LookupCategoryOp(name, raw)
	if !ok {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: db operation %s", dbstore.ErrUnsupported, name)
	}
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return op.Run(ctx, h, raw, a)
}

// completePage refuses a truncated or continued page.
func completePage(out dblifecycle.Outcome, what string) error {
	var pg struct {
		Truncated bool            `json:"truncated"`
		Cursor    json.RawMessage `json:"cursor"`
	}
	if out.Page != nil {
		if err := decode(out.Page, &pg); err != nil {
			return fmt.Errorf("%w: %s page: %v", dbstore.ErrIntegrity, what, err)
		}
	}
	if pg.Truncated || (len(pg.Cursor) != 0 && string(pg.Cursor) != "null") {
		return fmt.Errorf("%w: %s exceeds the validation cap", dbstore.ErrLimitExceeded, what)
	}
	return nil
}

// memoryOnlyOn checks, on an already admitted handle, that the database holds
// exactly the memory collection.
func memoryOnlyOn(ctx context.Context, h dbstore.Handle) error {
	out, err := outcomeOn(ctx, h, dbdoc.OpCollectionList, dbdoc.Input{Limit: dbdoc.MaxLimit})
	if err != nil {
		return err
	}
	if err := completePage(out, "collection list"); err != nil {
		return err
	}
	return memoryOnlyList(out.Result)
}

// MaintainRequest is one typed maintenance operation. CLI and MCP both build
// it from the same flags.
type MaintainRequest struct {
	Verb    string
	Target  string // S11 db locator for sqlite:file:PATH or sqlite:mem:SESSION/NAME
	Session string // explicit owner session for a persistent target
	Dir     string // resolves a relative persistent path
	// Index optionally names the bleve rank index to reconcile after a
	// committed change (supersede, expire, delete, import, restore).
	Index string
	ID    string
	// candidates: AsOf (RFC 3339, default now); StaleBefore (RFC 3339) makes a
	// still-valid candidate record last updated before it a stale candidate.
	AsOf        string
	StaleBefore string
	// supersede, expire, delete: the revision the change is based on.
	IfRevision          int64
	Replacement         string
	ReplacementRevision int64
	// export: Out directory; import: In directory; backup: Out file;
	// restore: In file.
	Out        string
	In         string
	OnConflict string
	Replace    bool
	Yes        bool
	RequestID  string
	Wait       time.Duration
}

// Candidate is one record a maintainer may act on. Of names the kept record
// for a duplicate.
type Candidate struct {
	ID       string       `json:"id"`
	Reason   string       `json:"reason"`
	Revision int64        `json:"revision"`
	State    kblife.State `json:"state"`
	Updated  string       `json:"updated"`
	Of       string       `json:"of,omitempty"`
	Type     string       `json:"type"`
	Title    string       `json:"title,omitempty"`
}

// CandidateReport is the result of the read-only candidates verb.
type CandidateReport struct {
	AsOf       string         `json:"as_of"`
	Records    int            `json:"records"`
	Counts     map[string]int `json:"counts"`
	Candidates []Candidate    `json:"candidates"`
}

// Maintain runs one maintenance verb and returns its envelope. It never
// panics on caller input and never creates a target.
func Maintain(ctx context.Context, req MaintainRequest) Envelope {
	env := Envelope{SchemaVersion: MaintenanceSchema, OK: true, Op: "memory.maintain." + req.Verb,
		Collection: Collection, MutationClass: maintainClass(req.Verb), RequestID: req.RequestID}
	if err := maintain(ctx, req, &env); err != nil {
		var ee *envError
		if errors.As(err, &ee) {
			env.SchemaVersion, env.OK, env.Result, env.Revision, env.Error = ErrorSchema, false, nil, nil, ee.inner.Error
			return env
		}
		var ce *committedError
		if errors.As(err, &ce) {
			env = fail(env, ce.cause)
			env.Error.Details = map[string]any{"committed": true, "recover": ce.recover}
			env.Error.SafeToRetry = false
			return env
		}
		return fail(env, err)
	}
	return env
}

// committedError reports a change that DID commit but whose follow-up check
// failed. It is never a plain success and never a retry-safe failure.
type committedError struct {
	cause   error
	recover string
}

func (e *committedError) Error() string { return e.cause.Error() }

func maintainClass(verb string) string {
	switch verb {
	case "candidates", "export", "backup":
		return "read"
	}
	return "write"
}

func maintain(ctx context.Context, req MaintainRequest, env *Envelope) error {
	if !slices.Contains(MaintainVerbs, req.Verb) {
		return fmt.Errorf("%w: maintain verb %q (want %s)", dbstore.ErrInvalidInput, req.Verb, strings.Join(MaintainVerbs, "|"))
	}
	if err := checkMaintainFlags(req); err != nil {
		return err
	}
	base := Request{Verb: "select", Target: req.Target, Session: req.Session, Dir: req.Dir, RequestID: req.RequestID, Wait: req.Wait}
	t, err := parseTarget(base)
	if err != nil {
		return err
	}
	id, err := dbstore.Resolve(t)
	if err != nil {
		return err
	}
	info := dblifecycle.ViewTarget(id, "")
	env.Target = &info
	if req.Index != "" {
		if err := checkIndexTarget(req); err != nil {
			return err
		}
	}
	switch req.Verb {
	case "candidates":
		return candidates(ctx, req, t, env)
	case "supersede":
		return viaCapture(ctx, req, env)
	case "expire":
		return expire(ctx, req, base, t, env)
	case "delete":
		return remove(ctx, req, base, t, env)
	case "export":
		return export(ctx, req, base, t, env)
	case "import":
		return importDir(ctx, req, base, t, env)
	case "backup":
		return backup(ctx, req, base, t, env)
	default:
		return restore(ctx, req, base, t, env)
	}
}

func checkIndexTarget(req MaintainRequest) error {
	_, _, err := parseRetrieveTargets(RetrieveQuery{Target: req.Target, Index: req.Index, Session: req.Session, Dir: req.Dir})
	return err
}

func checkMaintainFlags(req MaintainRequest) error {
	bad := func(flag, only string) error {
		return fmt.Errorf("%w: %s applies only to %s", dbstore.ErrInvalidInput, flag, only)
	}
	takesID := req.Verb == "supersede" || req.Verb == "expire" || req.Verb == "delete"
	if takesID != (req.ID != "") {
		return fmt.Errorf("%w: supersede, expire and delete take exactly one record ID; other verbs take none", dbstore.ErrInvalidInput)
	}
	switch {
	case !takesID && req.IfRevision != 0:
		return bad("--if-revision", "supersede, expire and delete")
	case takesID && req.IfRevision < 1:
		return fmt.Errorf("%w: %s requires --if-revision N (the revision inspect reported)", dbstore.ErrInvalidInput, req.Verb)
	case req.Verb != "supersede" && (req.Replacement != "" || req.ReplacementRevision != 0):
		return bad("--replacement", "supersede")
	case req.Verb != "candidates" && (req.AsOf != "" || req.StaleBefore != ""):
		return bad("--as-of/--stale-before", "candidates")
	case req.Verb != "export" && req.Verb != "backup" && req.Out != "":
		return bad("--out", "export and backup")
	case req.Verb != "import" && req.Verb != "restore" && req.In != "":
		return bad("--in", "import and restore")
	case req.Verb != "import" && req.OnConflict != "":
		return bad("--on-conflict", "import")
	case req.Verb != "import" && req.Verb != "restore" && req.Replace:
		return bad("--replace", "import and restore")
	case req.Verb != "import" && req.Verb != "restore" && req.Verb != "delete" && req.Yes:
		return bad("--yes", "delete, import and restore")
	case req.Verb == "candidates" || req.Verb == "export" || req.Verb == "backup":
		if req.Index != "" {
			return bad("--index", "verbs that change the collection")
		}
	}
	if (req.Verb == "backup" || req.Verb == "restore") && req.RequestID != "" {
		return fmt.Errorf("%w: --request-id is not supported for %s (the native snapshot operation has no replay record)", dbstore.ErrUnsupported, req.Verb)
	}
	if req.Verb == "delete" && !req.Yes {
		return fmt.Errorf("%w: delete removes the record permanently; pass --yes (forget hides it instead)", dbstore.ErrInvalidInput)
	}
	return nil
}

// ---- candidates ------------------------------------------------------------

func candidates(ctx context.Context, req MaintainRequest, t dbstore.Target, env *Envelope) error {
	asOf := clock().UTC()
	if s := strings.TrimSpace(req.AsOf); s != "" {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return fmt.Errorf("%w: --as-of %q is not RFC 3339", dbstore.ErrInvalidInput, req.AsOf)
		}
		asOf = v
	}
	var staleBefore time.Time
	if s := strings.TrimSpace(req.StaleBefore); s != "" {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return fmt.Errorf("%w: --stale-before %q is not RFC 3339", dbstore.ErrInvalidInput, req.StaleBefore)
		}
		staleBefore = v
	}
	snap, err := snapshotSource(ctx, RetrieveQuery{Target: req.Target, Session: req.Session, Dir: req.Dir, Wait: req.Wait}, t, MaxSyncRecords, MaxSyncBytes)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(snap))
	for id := range snap {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	rep := CandidateReport{AsOf: asOf.Format(time.RFC3339), Records: len(snap), Counts: map[string]int{ReasonExpired: 0, ReasonStale: 0, ReasonDuplicate: 0}, Candidates: []Candidate{}}
	type member struct {
		rec Record
		rev int64
	}
	groups := map[string][]member{}
	add := func(r Record, rev int64, reason, of string, s kblife.State) {
		rep.Candidates = append(rep.Candidates, Candidate{ID: r.ID, Reason: reason, Revision: rev, State: s, Updated: r.Updated, Of: of, Type: r.Type, Title: r.Title})
		rep.Counts[reason]++
	}
	for _, id := range ids {
		var r Record
		if err := json.Unmarshal(snap[id].raw, &r); err != nil {
			return fmt.Errorf("%w: memory record %q: %v", dbstore.ErrIntegrity, id, err)
		}
		rev, s := snap[id].revision, r.State(asOf)
		switch {
		case s == kblife.StateExpired:
			add(r, rev, ReasonExpired, "", s)
		case s == kblife.StateValid:
			if !staleBefore.IsZero() && r.Status == kb.StatusCandidate {
				if u, err := time.Parse(time.RFC3339, r.Updated); err == nil && u.Before(staleBefore) {
					add(r, rev, ReasonStale, "", s)
				}
			}
			k := dupKey(r)
			groups[k] = append(groups[k], member{r, rev})
		}
	}
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		slices.SortFunc(g, func(a, b member) int {
			if c := strings.Compare(a.rec.Created, b.rec.Created); c != 0 {
				return c
			}
			return strings.Compare(a.rec.ID, b.rec.ID)
		})
		for _, m := range g[1:] {
			add(m.rec, m.rev, ReasonDuplicate, g[0].rec.ID, kblife.StateValid)
		}
	}
	order := map[string]int{ReasonExpired: 0, ReasonStale: 1, ReasonDuplicate: 2}
	slices.SortFunc(rep.Candidates, func(a, b Candidate) int {
		if a.Reason != b.Reason {
			return order[a.Reason] - order[b.Reason]
		}
		return strings.Compare(a.ID, b.ID)
	})
	env.Result = rep
	return nil
}

// dupKey is the content identity of a duplicate: type plus the body with
// whitespace collapsed. Tags, scope and provenance do not make it distinct.
func dupKey(r Record) string {
	sum := sha256.Sum256([]byte(r.Type + "\x00" + strings.Join(strings.Fields(r.Body), " ")))
	return hex.EncodeToString(sum[:])
}

// ---- explicit changes ------------------------------------------------------

// viaCapture runs supersede through the capture verb, so there is one
// supersede semantics.
func viaCapture(ctx context.Context, req MaintainRequest, env *Envelope) error {
	c := Run(ctx, Request{Verb: "supersede", Target: req.Target, Session: req.Session, Dir: req.Dir, ID: req.ID,
		IfRevision: req.IfRevision, Replacement: req.Replacement, ReplacementRevision: req.ReplacementRevision,
		RequestID: req.RequestID, Wait: req.Wait})
	if !c.OK {
		return &envError{c}
	}
	env.Revision = c.Revision
	env.Result = asMap(c.Result)
	syncAfter(ctx, req, env)
	return nil
}

// envError carries a failed inner envelope's error unchanged.
type envError struct{ inner Envelope }

func (e *envError) Error() string { return e.inner.Error.Message }

func expire(ctx context.Context, req MaintainRequest, base Request, t dbstore.Target, env *Envelope) error {
	res, err := docOp(ctx, base, t, OpExpire, opInput{RequestID: req.RequestID, ID: req.ID, IfRevision: req.IfRevision}, false)
	if err != nil {
		return err
	}
	if err := finish(env, res); err != nil {
		return err
	}
	env.Result = asMap(env.Result)
	syncAfter(ctx, req, env)
	return nil
}

// runExpire is memory.expire: it closes one still-valid record's interval at
// an instant taken inside execution, under the record's revision.
func runExpire(ctx context.Context, h dbstore.Handle, in opInput) (dblifecycle.Outcome, error) {
	if in.ID == "" || in.IfRevision < 1 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: expire needs a record ID and --if-revision", dbstore.ErrInvalidInput)
	}
	r, rev, err := getOn(ctx, h, in.ID)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if rev != in.IfRevision {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: revision mismatch on record %q: expected_revision %d, actual_revision %d", dbstore.ErrConflict, in.ID, in.IfRevision, rev)
	}
	at := clock().UTC().Truncate(time.Second)
	if s := r.State(at); s != kblife.StateValid {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: record %s is %s, only a valid record can be expired", dbstore.ErrInvalidInput, r.ID, s)
	}
	v := r.Validity()
	v.ValidTo = at
	if err := v.Check(); err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	now := at.Format(time.RFC3339)
	r.ValidTo, r.Updated = now, now
	patch, err := json.Marshal(map[string]any{"valid_to": r.ValidTo, "updated": now})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	res, err := onHandle(ctx, h, dbdoc.OpPatch, dbdoc.Input{Collection: Collection, ID: r.ID, Patch: patch, IfRevision: &rev})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var w struct {
		Revision int64 `json:"revision"`
	}
	if err := decode(res, &w); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return outcome(w.Revision, map[string]any{"record": r, "state": kblife.StateExpired}), nil
}

func remove(ctx context.Context, req MaintainRequest, base Request, t dbstore.Target, env *Envelope) error {
	res, err := docOp(ctx, base, t, OpDelete, opInput{RequestID: req.RequestID, ID: req.ID, IfRevision: req.IfRevision}, true)
	if err != nil {
		return err
	}
	if err := finish(env, res); err != nil {
		return err
	}
	env.Result = asMap(env.Result)
	syncAfter(ctx, req, env)
	return nil
}

// runDelete is memory.delete: a revision-guarded physical removal. The
// record, its revision history and provenance are gone; the derived index
// follows on the next sync.
func runDelete(ctx context.Context, h dbstore.Handle, in opInput) (dblifecycle.Outcome, error) {
	if in.ID == "" || in.IfRevision < 1 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: delete needs a record ID and --if-revision", dbstore.ErrInvalidInput)
	}
	rev := in.IfRevision
	res, err := onHandle(ctx, h, dbdoc.OpRm, dbdoc.Input{Collection: Collection, ID: in.ID, IfRevision: &rev})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var w struct {
		Revision int64 `json:"revision"`
	}
	_ = decode(res, &w)
	return outcome(w.Revision, map[string]any{"id": in.ID, "deleted": true}), nil
}

// ---- export / import / backup / restore ------------------------------------

func absUnder(dir, p string) string {
	if p != "" && !filepath.IsAbs(p) && dir != "" {
		return filepath.Join(dir, p)
	}
	return p
}

func memoryOnlyList(res any) error {
	var list struct {
		Collections []struct {
			Name string `json:"name"`
		} `json:"collections"`
	}
	if err := decode(res, &list); err != nil {
		return err
	}
	found := false
	for _, c := range list.Collections {
		if c.Name != Collection {
			return fmt.Errorf("%w: database holds collection %q besides %q; kb memory export/backup/restore handle a memory-only database", dbstore.ErrInvalidInput, c.Name, Collection)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("%w: target has no memory collection; run `kb memory init` on it explicitly", dbstore.ErrNotFound)
	}
	return nil
}

func export(ctx context.Context, req MaintainRequest, base Request, t dbstore.Target, env *Envelope) error {
	out := absUnder(req.Dir, req.Out)
	res, err := docOp(ctx, base, t, OpExport, dbdoc.Input{Out: out}, false)
	if err != nil {
		return err
	}
	m := asMap(res)
	m["out"] = out
	env.Result = m
	return nil
}

func importDir(ctx context.Context, req MaintainRequest, base Request, t dbstore.Target, env *Envelope) error {
	src := absUnder(req.Dir, req.In)
	tmp, err := os.MkdirTemp("", "kbmem-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	in := filepath.Join(tmp, "export")
	if err := stageImportDir(ctx, src, in); err != nil {
		return err
	}
	if err := checkImportDir(ctx, in); err != nil {
		return err
	}
	if req.Replace && !req.Yes {
		return fmt.Errorf("%w: import --replace discards the target's collection; pass --yes", dbstore.ErrInvalidInput)
	}
	res, err := docOp(ctx, base, t, dbdoc.OpImport, dbdoc.Input{In: in, OnConflict: req.OnConflict, Replace: req.Replace}, req.Yes)
	if err != nil {
		return err
	}
	out := asMap(res)
	out["in"] = src
	env.Result = out
	syncAfter(ctx, req, env)
	return nil
}

// stageImportDir copies the manifest and its record file into a private
// directory so validation and import read the same bytes; replacing the
// source afterwards cannot change what is imported. Malformed shapes are left
// for checkImportDir to refuse on the staged copy.
func stageImportDir(ctx context.Context, src, dst string) error {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	mp := filepath.Join(src, dbdoc.ManifestName)
	info, err := os.Lstat(mp)
	if err != nil {
		return fmt.Errorf("%w: import manifest: %v", dbstore.ErrNotFound, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return fmt.Errorf("%w: %s must be a regular file under %d bytes", dbstore.ErrMalformedInput, dbdoc.ManifestName, maxManifestBytes)
	}
	if err := copyBounded(ctx, mp, filepath.Join(dst, dbdoc.ManifestName), maxManifestBytes); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dst, dbdoc.ManifestName))
	if err != nil {
		return err
	}
	var m struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Files) != 1 {
		return nil
	}
	p := m.Files[0].Path
	if !filepath.IsLocal(p) || strings.ContainsAny(p, `/\`) || p == dbdoc.ManifestName {
		return nil
	}
	rp := filepath.Join(src, p)
	if fi, err := os.Lstat(rp); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	return copyBounded(ctx, rp, filepath.Join(dst, p), maxImportBytes+1)
}

const (
	maxManifestBytes = 16 << 20
	maxImportBytes   = 256 << 20
)

var memoryID = regexp.MustCompile(`^mem-[0-9a-f]{24}$`)

// checkImportDir validates an export directory as memory content before the
// import opens the target: a bounded manifest read, exactly the memory
// collection, and every record a well-formed memory record whose line ID is
// its document ID. dbdoc.OpImport then re-verifies counts, bytes and sha256.
func checkImportDir(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return dbstore.FromContext(err)
	}
	p := filepath.Join(dir, dbdoc.ManifestName)
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: import manifest: %v", dbstore.ErrNotFound, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return fmt.Errorf("%w: %s must be a regular file under %d bytes", dbstore.ErrMalformedInput, dbdoc.ManifestName, maxManifestBytes)
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("%w: import manifest: %v", dbstore.ErrNotFound, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	f.Close()
	if err != nil {
		return err
	}
	if len(raw) > maxManifestBytes {
		return fmt.Errorf("%w: %s over %d bytes", dbstore.ErrMalformedInput, dbdoc.ManifestName, maxManifestBytes)
	}
	var m struct {
		SchemaVersion string `json:"schema_version"`
		SchemaDef     struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"schema_def"`
		Files []struct {
			Path       string `json:"path"`
			Collection string `json:"collection"`
			Bytes      int64  `json:"bytes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.SchemaVersion != dbdoc.ExportSchema {
		return fmt.Errorf("%w: %s is not a %s document export", dbstore.ErrInvalidInput, dir, dbdoc.ExportSchema)
	}
	if len(m.SchemaDef.Collections) != 1 || m.SchemaDef.Collections[0].Name != Collection || len(m.Files) != 1 || m.Files[0].Collection != Collection {
		return fmt.Errorf("%w: a memory import takes an export of exactly the %q collection", dbstore.ErrInvalidInput, Collection)
	}
	rf := m.Files[0]
	if !filepath.IsLocal(rf.Path) || strings.ContainsAny(rf.Path, `/\`) || rf.Path == dbdoc.ManifestName {
		return fmt.Errorf("%w: record file path %q", dbstore.ErrMalformedInput, rf.Path)
	}
	rp := filepath.Join(dir, rf.Path)
	fi, err := os.Lstat(rp)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: record file %s is missing or not a regular file", dbstore.ErrMalformedInput, rf.Path)
	}
	if fi.Size() > maxImportBytes {
		return fmt.Errorf("%w: record file %s is over %d bytes", dbstore.ErrLimitExceeded, rf.Path, maxImportBytes)
	}
	file, err := os.Open(rp)
	if err != nil {
		return err
	}
	defer file.Close()
	sc := bufio.NewScanner(io.LimitReader(file, maxImportBytes+1))
	sc.Buffer(make([]byte, 64<<10), dbdoc.MaxRecordBytes+1)
	seen := map[string]bool{}
	for n := 1; sc.Scan(); n++ {
		if n%256 == 0 {
			if err := ctx.Err(); err != nil {
				return dbstore.FromContext(err)
			}
		}
		var line struct {
			ID       string          `json:"id"`
			Revision int64           `json:"revision"`
			Doc      json.RawMessage `json:"doc"`
		}
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&line); err != nil || line.Revision < 1 {
			return fmt.Errorf("%w: %s record %d is not an export record", dbstore.ErrMalformedInput, rf.Path, n)
		}
		if seen[line.ID] {
			return fmt.Errorf("%w: %s repeats record %q", dbstore.ErrMalformedInput, rf.Path, line.ID)
		}
		seen[line.ID] = true
		if err := validateRecordDoc(line.ID, line.Doc); err != nil {
			return fmt.Errorf("%s record %d: %w", rf.Path, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("%w: a record is over %d bytes", dbstore.ErrLimitExceeded, dbdoc.MaxRecordBytes)
		}
		return err
	}
	return nil
}

// validateRecordDoc checks that a stored document is a memory record under
// the given ID: strict fields, the minted ID shape, a known form and type, a
// body, and parseable timestamps.
func validateRecordDoc(id string, doc []byte) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: memory record %q: %s", dbstore.ErrInvalidInput, id, fmt.Sprintf(format, a...))
	}
	var r Record
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return bad("not a memory record (%v)", err)
	}
	switch {
	case !memoryID.MatchString(id) || r.ID != id:
		return bad("id must be a mem- id equal to the document id")
	case !kb.ValidForm(r.Form) || checkType(r.Type) != nil:
		return bad("unknown form or type")
	case checkBody(r.Body) != nil || r.Status == "":
		return bad("needs a body and a status")
	}
	for _, ts := range []string{r.Created, r.Updated} {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return bad("created/updated must be RFC 3339")
		}
	}
	for _, ts := range []string{r.ValidFrom, r.ValidTo} {
		if ts == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return bad("valid_from/valid_to must be RFC 3339")
		}
	}
	return nil
}

// checkSnapshot opens a native snapshot read-only and requires it to be a
// memory-only database of well-formed records, so restore never replaces a
// good target with foreign or malformed content.
func checkSnapshot(ctx context.Context, in string, wait time.Duration) error {
	abs, err := filepath.Abs(in)
	if err != nil {
		return err
	}
	h, err := (sqlitedb.Adapter{}).Open(ctx, dbstore.Target{Engine: Engine, Mode: dbstore.Persistent, Path: abs}, dbstore.Options{ReadOnly: true, Wait: wait})
	if err != nil {
		return err
	}
	defer h.Close()
	if err := memoryOnlyOn(ctx, h); err != nil {
		return fmt.Errorf("snapshot %s: %w", in, err)
	}
	out, err := outcomeOn(ctx, h, dbdoc.OpFind, dbdoc.Input{Collection: Collection, Limit: snapshotRecordCap, MaxBytes: MaxSyncBytes})
	if err != nil {
		return err
	}
	if err := completePage(out, fmt.Sprintf("snapshot %s (%d records, %d bytes)", in, snapshotRecordCap, MaxSyncBytes)); err != nil {
		return err
	}
	var res struct {
		Documents []dbdoc.Document `json:"documents"`
	}
	if err := decode(out.Result, &res); err != nil {
		return fmt.Errorf("%w: snapshot %s: %v", dbstore.ErrIntegrity, in, err)
	}
	for _, d := range res.Documents {
		if err := validateRecordDoc(d.ID, d.Doc); err != nil {
			return fmt.Errorf("snapshot %s: %w", in, err)
		}
	}
	return nil
}

// lifecycleOp runs a native owner/direct operation (backup, restore).
func lifecycleOp(ctx context.Context, req MaintainRequest, t dbstore.Target, op, out, in string, replace bool) error {
	s := session(Request{Session: req.Session}, t)
	if s != "" {
		res, err := dblifecycle.Call(ctx, s, dblifecycle.Request{Op: op, Target: t, Wait: req.Wait, Out: out, In: in, Replace: replace, Yes: req.Yes})
		if err != nil {
			return err
		}
		if !res.OK {
			if res.Error == nil {
				return fmt.Errorf("%w: owner refused %s without an error", dbstore.ErrBackend, op)
			}
			return &ownerError{*res.Error}
		}
		return nil
	}
	var err error
	if op == "backup" {
		_, err = dblifecycle.Service{}.Backup(ctx, t, out, req.Wait)
	} else {
		_, err = dblifecycle.Service{}.Restore(ctx, t, in, replace, req.Wait)
	}
	return err
}

// backup writes the native snapshot to a private sibling file, validates
// those exact bytes as memory-only, and only then publishes it under the
// requested name without clobbering. A foreign collection never leaves under
// the final name, and no check/snapshot race exists.
func backup(ctx context.Context, req MaintainRequest, _ Request, t dbstore.Target, env *Envelope) error {
	out := absUnder(req.Dir, req.Out)
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%w: backup target %s already exists", dbstore.ErrExists, out)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(out), ".kbmem-backup-")
	if err != nil {
		return fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, "snapshot.db")
	if err := lifecycleOp(ctx, req, t, "backup", staged, "", false); err != nil {
		return err
	}
	if err := checkSnapshot(ctx, staged, req.Wait); err != nil {
		return err
	}
	if err := os.Link(staged, out); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: backup target %s already exists", dbstore.ErrExists, out)
		}
		return fmt.Errorf("%w: publish backup: %v", dbstore.ErrBackend, err)
	}
	env.Result = map[string]any{"backup": out}
	return nil
}

// copyBounded copies src into a private file so validation and restore read
// the same immutable bytes; a source that changes after the copy is moot.
func copyBounded(ctx context.Context, src, dst string, limit int64) error {
	f, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: snapshot %s", dbstore.ErrNotFound, src)
		}
		return fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	defer f.Close()
	w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer w.Close()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return dbstore.FromContext(err)
		}
		k, rerr := f.Read(buf)
		if k > 0 {
			if n += int64(k); n > limit {
				return fmt.Errorf("%w: snapshot %s exceeds %d bytes", dbstore.ErrLimitExceeded, src, limit)
			}
			if _, err := w.Write(buf[:k]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

const maxSnapshotBytes = 1 << 30

var snapshotRecordCap = MaxSyncRecords

func restore(ctx context.Context, req MaintainRequest, _ Request, t dbstore.Target, env *Envelope) error {
	if !req.Yes {
		return fmt.Errorf("%w: restore replaces the target's contents; pass --yes", dbstore.ErrInvalidInput)
	}
	in := absUnder(req.Dir, req.In)
	tmp, err := os.MkdirTemp("", "kbmem-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	copied := filepath.Join(tmp, "snapshot.db")
	if err := copyBounded(ctx, in, copied, maxSnapshotBytes); err != nil {
		return err
	}
	if err := checkSnapshot(ctx, copied, req.Wait); err != nil {
		return err
	}
	if err := lifecycleOp(ctx, req, t, "restore", "", copied, true); err != nil {
		return err
	}
	res := map[string]any{"restored": in}
	c := Run(ctx, Request{Verb: "select", Target: req.Target, Session: req.Session, Dir: req.Dir, Wait: req.Wait})
	if !c.OK {
		return &committedError{cause: fmt.Errorf("%w: restore committed but verification failed: %s", dbstore.ErrIntegrity, c.Error.Message),
			recover: "the snapshot replaced the target; run `kb memory select` and inspect the target, or restore a known-good backup"}
	}
	res["verified"] = c.Result
	env.Result = res
	syncAfter(ctx, req, env)
	return nil
}

// ---- index pipeline --------------------------------------------------------

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	if v == nil {
		return m
	}
	if b, err := json.Marshal(v); err == nil && json.Unmarshal(b, &m) == nil {
		return m
	}
	return map[string]any{"result": v}
}

// syncAfter runs the explicit SyncIndex pipeline after a committed change. A
// failed sync never turns the committed change into an error a caller would
// retry: the result carries the sync error and the recovery instead.
func syncAfter(ctx context.Context, req MaintainRequest, env *Envelope) {
	if req.Index == "" {
		return
	}
	m := asMap(env.Result)
	env.Result = m
	m["committed"] = true
	s := SyncIndex(ctx, SyncQuery{Target: req.Target, Index: req.Index, Session: req.Session, Dir: req.Dir, Wait: req.Wait})
	if s.OK {
		m["index"] = s.Result
		return
	}
	m["index_error"] = s.Error
	m["recover"] = "the change committed; rerun index-sync for this TARGET and --index (recall drops hits the collection no longer holds until then)"
}
