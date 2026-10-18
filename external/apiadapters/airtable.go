package apiadapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/external/saasclients"
)

// AirtableAPIDoc is the public contract this adapter is implemented from
// (no n8n source involved).
const AirtableAPIDoc = "https://airtable.com/developers/web/api/introduction"

// Airtable is the typed adapter for the record operations the catalog
// needs where no native CLI exists: list, get, create, update, delete.
// Transport, bounds and error codes ride saasclients.API with Client
// "airtable" (error shape extended there). Batch writes, attachments,
// views management and webhooks stay generic-HTTP/unsupported.
type Airtable struct {
	API      saasclients.API
	Ledger   *saasclients.Ledger
	Throttle *Throttle
}

// NewAirtable builds the adapter over an explicit endpoint (the Airtable
// API base in use, or a loopback fake in tests) and personal-access token.
func NewAirtable(baseURL, token string) *Airtable {
	return &Airtable{
		API:      saasclients.API{Client: "airtable", BaseURL: baseURL, Token: token},
		Ledger:   &saasclients.Ledger{},
		Throttle: NewThrottle(250 * time.Millisecond),
	}
}

// AirtableRecord is one record. Fields stay raw JSON: the provider owns
// the field schema and this adapter preserves precision (numbers, nulls)
// with no coercion.
type AirtableRecord struct {
	ID      string          `json:"id"`
	Created string          `json:"createdTime,omitempty"`
	Fields  json.RawMessage `json:"fields,omitempty"`
}

// AirtableRecordListInput lists one record page. PageSize caps one
// provider page (the provider maximum is 100); Offset travels verbatim.
type AirtableRecordListInput struct {
	SchemaVersion   string `json:"schema_version"`
	Base            string `json:"base"`
	Table           string `json:"table"`
	PageSize        int    `json:"page_size,omitempty"`
	Offset          string `json:"offset,omitempty"`
	FilterByFormula string `json:"filter_by_formula,omitempty"`
	View            string `json:"view,omitempty"`
}

// AirtableRecordListOutput is one versioned record page. Offset is empty
// when the listing is complete.
type AirtableRecordListOutput struct {
	SchemaVersion string           `json:"schema_version"`
	Records       []AirtableRecord `json:"records"`
	Offset        string           `json:"offset,omitempty"`
	Binding       gws.Binding      `json:"-"`
}

// AirtableRecordGetInput retrieves one record by id.
type AirtableRecordGetInput struct {
	SchemaVersion string `json:"schema_version"`
	Base          string `json:"base"`
	Table         string `json:"table"`
	RecordID      string `json:"record_id"`
}

// AirtableRecordCreateInput creates one record.
type AirtableRecordCreateInput struct {
	SchemaVersion string          `json:"schema_version"`
	Base          string          `json:"base"`
	Table         string          `json:"table"`
	Fields        json.RawMessage `json:"fields"`
	Key           string          `json:"key,omitempty"`
	DryRun        bool            `json:"dry_run,omitempty"`
}

// AirtableRecordUpdateInput replaces one record's fields (by id).
type AirtableRecordUpdateInput struct {
	SchemaVersion string          `json:"schema_version"`
	Base          string          `json:"base"`
	Table         string          `json:"table"`
	RecordID      string          `json:"record_id"`
	Fields        json.RawMessage `json:"fields"`
	DryRun        bool            `json:"dry_run,omitempty"`
}

// AirtableRecordDeleteInput deletes one record by id.
type AirtableRecordDeleteInput struct {
	SchemaVersion string `json:"schema_version"`
	Base          string `json:"base"`
	Table         string `json:"table"`
	RecordID      string `json:"record_id"`
	DryRun        bool   `json:"dry_run,omitempty"`
}

// MaxAirtablePageSize is the provider's per-page maximum.
const MaxAirtablePageSize = 100

func (a *Airtable) path(base, table, id string) (string, *gws.Error) {
	if base == "" || table == "" {
		return "", gws.NewError(gws.CodeInvalidInput, "airtable: base and table are required", nil)
	}
	p := url.PathEscape(base) + "/" + url.PathEscape(table)
	if id != "" {
		p += "/" + url.PathEscape(id)
	}
	return p, nil
}

// RecordList reads one record page (bounded retry, no mutation).
func (a *Airtable) RecordList(ctx context.Context, in AirtableRecordListInput, b, issuedFor gws.Binding) (AirtableRecordListOutput, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "record-list", "airtable"); err != nil {
		return AirtableRecordListOutput{}, err
	}
	p, err := a.path(in.Base, in.Table, "")
	if err != nil {
		return AirtableRecordListOutput{}, err
	}
	if err := CheckCursor(b, issuedFor, in.Offset); err != nil {
		return AirtableRecordListOutput{}, err
	}
	size := in.PageSize
	if size <= 0 {
		size = gws.DefaultPageSize
	}
	if size > MaxAirtablePageSize {
		return AirtableRecordListOutput{}, gws.NewError(gws.CodeOutputLimit,
			fmt.Sprintf("airtable record-list: page_size %d exceeds provider max %d", size, MaxAirtablePageSize), nil)
	}
	q := map[string]string{
		"pageSize":        fmt.Sprint(size),
		"offset":          in.Offset,
		"filterByFormula": in.FilterByFormula,
		"view":            in.View,
	}
	r, err := doRead(ctx, a.API, a.Throttle, "GET", p, q, nil, nil, 0)
	if err != nil {
		return AirtableRecordListOutput{}, err
	}
	var res struct {
		Records []AirtableRecord `json:"records"`
		Offset  string           `json:"offset"`
	}
	if jerr := json.Unmarshal(r.Body, &res); jerr != nil {
		return AirtableRecordListOutput{}, gws.NewError(gws.CodeProviderFailure, "airtable record-list: unreadable provider body", nil)
	}
	if res.Records == nil {
		res.Records = []AirtableRecord{}
	}
	return AirtableRecordListOutput{SchemaVersion: SchemaVersion, Records: res.Records, Offset: res.Offset, Binding: b}, nil
}

// RecordGet retrieves one record by id (bounded retry, no mutation).
func (a *Airtable) RecordGet(ctx context.Context, in AirtableRecordGetInput) (AirtableRecord, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "record-get", "airtable"); err != nil {
		return AirtableRecord{}, err
	}
	if in.RecordID == "" {
		return AirtableRecord{}, gws.NewError(gws.CodeInvalidInput, "airtable record-get: record_id is required", nil)
	}
	p, err := a.path(in.Base, in.Table, in.RecordID)
	if err != nil {
		return AirtableRecord{}, err
	}
	r, err := doRead(ctx, a.API, a.Throttle, "GET", p, nil, nil, nil, 0)
	if err != nil {
		return AirtableRecord{}, err
	}
	var rec AirtableRecord
	if jerr := json.Unmarshal(r.Body, &rec); jerr != nil || rec.ID == "" {
		return AirtableRecord{}, gws.NewError(gws.CodeProviderFailure, "airtable: unreadable record body", nil)
	}
	return rec, nil
}

// RecordCreate creates one record. No provider idempotency key exists, so
// the shared ledger claims the derived key and refuses a replay; a dry
// run validates and sends nothing. Never retried.
func (a *Airtable) RecordCreate(ctx context.Context, scope saasclients.Scope, in AirtableRecordCreateInput) (AirtableRecord, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "record-create", "airtable"); err != nil {
		return AirtableRecord{}, err
	}
	p, err := a.path(in.Base, in.Table, "")
	if err != nil {
		return AirtableRecord{}, err
	}
	if len(in.Fields) == 0 {
		return AirtableRecord{}, gws.NewError(gws.CodeInvalidInput, "airtable record-create: fields are required", nil)
	}
	body, _ := json.Marshal(map[string]any{"fields": json.RawMessage(in.Fields)})
	m := saasclients.Mutation{Scope: scope, Op: "record-create", Key: in.Key, Payload: body, DryRun: in.DryRun}
	plan, verr := m.Validate()
	if verr != nil {
		return AirtableRecord{}, verr
	}
	if in.DryRun {
		return AirtableRecord{}, nil
	}
	if prior, replay := a.Ledger.Begin(plan.Key); replay {
		return AirtableRecord{}, gws.NewError(gws.CodePartialMutation,
			"airtable: replayed idempotency key — not creating again; prior outcome "+prior, nil).
			WithOp("record-create", scope.Client+":"+m.Op)
	}
	r, derr := doMutate(ctx, a.API, a.Throttle, "POST", p, body, nil)
	if derr != nil {
		out := "failed"
		if derr.Code == gws.CodeTimeout || derr.Code == gws.CodeCancelled {
			out = "unknown"
		}
		a.Ledger.Finish(plan.Key, out)
		return AirtableRecord{}, derr
	}
	var rec AirtableRecord
	if jerr := json.Unmarshal(r.Body, &rec); jerr != nil || rec.ID == "" {
		a.Ledger.Finish(plan.Key, "unknown")
		return AirtableRecord{}, gws.NewError(gws.CodeProviderFailure, "airtable: unreadable created-record body; remote outcome unknown", nil)
	}
	a.Ledger.Finish(plan.Key, "confirmed")
	return rec, nil
}

// RecordUpdate replaces one record's fields. Keyed by record id,
// re-applying the same fields converges, so no ledger claim is needed;
// the mutation still validates before effects and never retries.
func (a *Airtable) RecordUpdate(ctx context.Context, scope saasclients.Scope, in AirtableRecordUpdateInput) (AirtableRecord, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "record-update", "airtable"); err != nil {
		return AirtableRecord{}, err
	}
	if in.RecordID == "" {
		return AirtableRecord{}, gws.NewError(gws.CodeInvalidInput, "airtable record-update: record_id is required", nil)
	}
	p, err := a.path(in.Base, in.Table, in.RecordID)
	if err != nil {
		return AirtableRecord{}, err
	}
	if len(in.Fields) == 0 {
		return AirtableRecord{}, gws.NewError(gws.CodeInvalidInput, "airtable record-update: fields are required", nil)
	}
	body, _ := json.Marshal(map[string]any{"fields": json.RawMessage(in.Fields)})
	m := saasclients.Mutation{Scope: scope, Op: "record-update", Payload: body, DryRun: in.DryRun}
	if _, verr := m.Validate(); verr != nil {
		return AirtableRecord{}, verr
	}
	if in.DryRun {
		return AirtableRecord{}, nil
	}
	r, derr := doMutate(ctx, a.API, a.Throttle, "PATCH", p, body, nil)
	if derr != nil {
		return AirtableRecord{}, derr
	}
	var rec AirtableRecord
	if jerr := json.Unmarshal(r.Body, &rec); jerr != nil || rec.ID == "" {
		return AirtableRecord{}, gws.NewError(gws.CodeProviderFailure, "airtable: unreadable updated-record body", nil)
	}
	return rec, nil
}

// RecordDelete deletes one record by id (cleanup path). Validates before
// effects and never retries; a repeat converges on typed not-found.
func (a *Airtable) RecordDelete(ctx context.Context, scope saasclients.Scope, in AirtableRecordDeleteInput) *gws.Error {
	if err := CheckVersion(in.SchemaVersion, "record-delete", "airtable"); err != nil {
		return err
	}
	if in.RecordID == "" {
		return gws.NewError(gws.CodeInvalidInput, "airtable record-delete: record_id is required", nil)
	}
	p, err := a.path(in.Base, in.Table, in.RecordID)
	if err != nil {
		return err
	}
	m := saasclients.Mutation{Scope: scope, Op: "record-delete", Payload: []byte(in.RecordID), DryRun: in.DryRun}
	if _, verr := m.Validate(); verr != nil {
		return verr
	}
	if in.DryRun {
		return nil
	}
	_, derr := doMutate(ctx, a.API, a.Throttle, "DELETE", p, nil, nil)
	return derr
}
