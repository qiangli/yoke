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

// NotionVersion pins the integration API version sent on every request.
// Upgrades are deliberate reviewed bumps: a silent drift would change
// property shapes under the typed output below.
const NotionVersion = "2022-06-28"

// NotionAPIDoc is the public contract this adapter is implemented from
// (no n8n source involved).
const NotionAPIDoc = "https://developers.notion.com/reference/intro"

// Notion is the typed adapter for the operations the catalog needs where
// no native CLI exists: page retrieve/create/update/archive and database
// query. Transport, bounds and error codes ride saasclients.API with
// Client "notion" (error shape extended there); the Notion-Version pin
// rides every request. Search, users, comments, file uploads and polling
// triggers stay generic-HTTP/unsupported (see package doc).
type Notion struct {
	API      saasclients.API
	Ledger   *saasclients.Ledger
	Throttle *Throttle
}

// NewNotion builds the adapter over an explicit endpoint (the Notion API
// base in use, or a loopback fake in tests) and integration token.
func NewNotion(baseURL, token string) *Notion {
	return &Notion{
		API:      saasclients.API{Client: "notion", BaseURL: baseURL, Token: token},
		Ledger:   &saasclients.Ledger{},
		Throttle: NewThrottle(350 * time.Millisecond),
	}
}

func (n *Notion) headers() map[string]string {
	return map[string]string{"Notion-Version": NotionVersion}
}

// NotionPage is one page object. Properties stay raw JSON: the provider
// owns the property schema and this adapter declares no lossy projection.
type NotionPage struct {
	ID         string          `json:"id"`
	Created    string          `json:"created_time,omitempty"`
	Edited     string          `json:"last_edited_time,omitempty"`
	Archived   bool            `json:"archived,omitempty"`
	URL        string          `json:"url,omitempty"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

// NotionParent is the typed create parent: exactly one of database or page.
type NotionParent struct {
	Type       string `json:"type"`
	DatabaseID string `json:"database_id,omitempty"`
	PageID     string `json:"page_id,omitempty"`
}

// NotionPageGetInput retrieves one page.
type NotionPageGetInput struct {
	SchemaVersion string `json:"schema_version"`
	PageID        string `json:"page_id"`
}

// NotionPageCreateInput creates one page under a database or page parent.
type NotionPageCreateInput struct {
	SchemaVersion string          `json:"schema_version"`
	Parent        NotionParent    `json:"parent"`
	Properties    json.RawMessage `json:"properties"`
	Key           string          `json:"key,omitempty"`
	DryRun        bool            `json:"dry_run,omitempty"`
}

// NotionPageUpdateInput replaces a page's properties (by id: convergent).
type NotionPageUpdateInput struct {
	SchemaVersion string          `json:"schema_version"`
	PageID        string          `json:"page_id"`
	Properties    json.RawMessage `json:"properties"`
	DryRun        bool            `json:"dry_run,omitempty"`
}

// NotionPageArchiveInput archives one page (the delete effect).
type NotionPageArchiveInput struct {
	SchemaVersion string `json:"schema_version"`
	PageID        string `json:"page_id"`
	DryRun        bool   `json:"dry_run,omitempty"`
}

// NotionQueryInput queries one database. PageSize caps one provider page
// (the provider maximum is 100); StartCursor travels verbatim.
type NotionQueryInput struct {
	SchemaVersion string          `json:"schema_version"`
	DatabaseID    string          `json:"database_id"`
	Filter        json.RawMessage `json:"filter,omitempty"`
	Sorts         json.RawMessage `json:"sorts,omitempty"`
	PageSize      int             `json:"page_size,omitempty"`
	StartCursor   string          `json:"start_cursor,omitempty"`
}

// NotionQueryOutput is one versioned result page.
type NotionQueryOutput struct {
	SchemaVersion string       `json:"schema_version"`
	Pages         []NotionPage `json:"pages"`
	NextCursor    string       `json:"next_cursor,omitempty"`
	HasMore       bool         `json:"has_more"`
	Binding       gws.Binding  `json:"-"`
}

// MaxNotionPageSize is the provider's per-page maximum.
const MaxNotionPageSize = 100

func (in NotionParent) validate() *gws.Error {
	switch in.Type {
	case "database_id":
		if in.DatabaseID == "" {
			return gws.NewError(gws.CodeInvalidInput, "notion: database parent needs database_id", nil)
		}
	case "page_id":
		if in.PageID == "" {
			return gws.NewError(gws.CodeInvalidInput, "notion: page parent needs page_id", nil)
		}
	default:
		return gws.NewError(gws.CodeInvalidInput, "notion: parent type must be database_id or page_id", nil)
	}
	return nil
}

// PageGet retrieves one page by id (bounded retry, no mutation).
func (n *Notion) PageGet(ctx context.Context, in NotionPageGetInput) (NotionPage, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "page-get", "notion"); err != nil {
		return NotionPage{}, err
	}
	if in.PageID == "" {
		return NotionPage{}, gws.NewError(gws.CodeInvalidInput, "notion page-get: page_id is required", nil)
	}
	return n.getPage(ctx, in.PageID)
}

func (n *Notion) getPage(ctx context.Context, id string) (NotionPage, *gws.Error) {
	r, err := doRead(ctx, n.API, n.Throttle, "GET", "pages/"+url.PathEscape(id), nil, nil, n.headers(), 0)
	if err != nil {
		return NotionPage{}, err
	}
	var p NotionPage
	if jerr := json.Unmarshal(r.Body, &p); jerr != nil || p.ID == "" {
		return NotionPage{}, gws.NewError(gws.CodeProviderFailure, "notion: unreadable page body", nil)
	}
	return p, nil
}

// PageCreate creates one page. No provider idempotency key exists, so the
// shared ledger claims the derived key and refuses a replay; a dry run
// validates and sends nothing. Never retried.
func (n *Notion) PageCreate(ctx context.Context, scope saasclients.Scope, in NotionPageCreateInput) (NotionPage, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "page-create", "notion"); err != nil {
		return NotionPage{}, err
	}
	if err := in.Parent.validate(); err != nil {
		return NotionPage{}, err
	}
	if len(in.Properties) == 0 {
		return NotionPage{}, gws.NewError(gws.CodeInvalidInput, "notion page-create: properties are required", nil)
	}
	body, _ := json.Marshal(map[string]any{"parent": in.Parent, "properties": json.RawMessage(in.Properties)})
	m := saasclients.Mutation{Scope: scope, Op: "page-create", Key: in.Key, Payload: body, DryRun: in.DryRun}
	return n.createPage(ctx, m, body)
}

func (n *Notion) createPage(ctx context.Context, m saasclients.Mutation, body []byte) (NotionPage, *gws.Error) {
	plan, err := m.Validate()
	if err != nil {
		return NotionPage{}, err
	}
	if m.DryRun {
		return NotionPage{}, nil
	}
	if prior, replay := n.Ledger.Begin(plan.Key); replay {
		return NotionPage{}, gws.NewError(gws.CodePartialMutation,
			"notion: replayed idempotency key — not creating again; prior outcome "+prior, nil).
			WithOp("page-create", m.Scope.Client+":"+m.Op)
	}
	r, derr := doMutate(ctx, n.API, n.Throttle, "POST", "pages", body, n.headers())
	if derr != nil {
		out := "failed"
		if derr.Code == gws.CodeTimeout || derr.Code == gws.CodeCancelled {
			out = "unknown"
		}
		n.Ledger.Finish(plan.Key, out)
		return NotionPage{}, derr
	}
	var p NotionPage
	if jerr := json.Unmarshal(r.Body, &p); jerr != nil || p.ID == "" {
		n.Ledger.Finish(plan.Key, "unknown")
		return NotionPage{}, gws.NewError(gws.CodeProviderFailure, "notion: unreadable created-page body; remote outcome unknown", nil)
	}
	n.Ledger.Finish(plan.Key, "confirmed")
	return p, nil
}

// PageUpdate replaces a page's properties. Keyed by page id, re-applying
// the same properties converges, so no ledger claim is needed; the
// mutation still validates before effects and never retries.
func (n *Notion) PageUpdate(ctx context.Context, scope saasclients.Scope, in NotionPageUpdateInput) (NotionPage, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "page-update", "notion"); err != nil {
		return NotionPage{}, err
	}
	if in.PageID == "" {
		return NotionPage{}, gws.NewError(gws.CodeInvalidInput, "notion page-update: page_id is required", nil)
	}
	if len(in.Properties) == 0 {
		return NotionPage{}, gws.NewError(gws.CodeInvalidInput, "notion page-update: properties are required", nil)
	}
	body, _ := json.Marshal(map[string]any{"properties": json.RawMessage(in.Properties)})
	m := saasclients.Mutation{Scope: scope, Op: "page-update", Payload: body, DryRun: in.DryRun}
	if _, err := m.Validate(); err != nil {
		return NotionPage{}, err
	}
	if in.DryRun {
		return NotionPage{}, nil
	}
	r, err := doMutate(ctx, n.API, n.Throttle, "PATCH", "pages/"+url.PathEscape(in.PageID), body, n.headers())
	if err != nil {
		return NotionPage{}, err
	}
	var p NotionPage
	if jerr := json.Unmarshal(r.Body, &p); jerr != nil || p.ID == "" {
		return NotionPage{}, gws.NewError(gws.CodeProviderFailure, "notion: unreadable updated-page body", nil)
	}
	return p, nil
}

// PageArchive archives one page (the delete effect; restorable
// provider-side, and the id makes a repeat explicit).
func (n *Notion) PageArchive(ctx context.Context, scope saasclients.Scope, in NotionPageArchiveInput) *gws.Error {
	if err := CheckVersion(in.SchemaVersion, "page-archive", "notion"); err != nil {
		return err
	}
	if in.PageID == "" {
		return gws.NewError(gws.CodeInvalidInput, "notion page-archive: page_id is required", nil)
	}
	m := saasclients.Mutation{Scope: scope, Op: "page-archive", Payload: []byte(in.PageID), DryRun: in.DryRun}
	if _, err := m.Validate(); err != nil {
		return err
	}
	if in.DryRun {
		return nil
	}
	body := []byte(`{"archived":true}`)
	_, err := doMutate(ctx, n.API, n.Throttle, "PATCH", "pages/"+url.PathEscape(in.PageID), body, n.headers())
	return err
}

// DatabaseQuery reads one database result page. The query itself travels
// by POST (provider contract) but is a read: bounded retry applies, and
// the start cursor travels verbatim under binding check.
func (n *Notion) DatabaseQuery(ctx context.Context, in NotionQueryInput, b, issuedFor gws.Binding) (NotionQueryOutput, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "database-query", "notion"); err != nil {
		return NotionQueryOutput{}, err
	}
	if in.DatabaseID == "" {
		return NotionQueryOutput{}, gws.NewError(gws.CodeInvalidInput, "notion database-query: database_id is required", nil)
	}
	if err := CheckCursor(b, issuedFor, in.StartCursor); err != nil {
		return NotionQueryOutput{}, err
	}
	size := in.PageSize
	if size <= 0 {
		size = gws.DefaultPageSize
	}
	if size > MaxNotionPageSize {
		return NotionQueryOutput{}, gws.NewError(gws.CodeOutputLimit,
			fmt.Sprintf("notion database-query: page_size %d exceeds provider max %d", size, MaxNotionPageSize), nil)
	}
	q := map[string]any{"page_size": size}
	if in.StartCursor != "" {
		q["start_cursor"] = in.StartCursor
	}
	if len(in.Filter) > 0 {
		q["filter"] = json.RawMessage(in.Filter)
	}
	if len(in.Sorts) > 0 {
		q["sorts"] = json.RawMessage(in.Sorts)
	}
	body, _ := json.Marshal(q)
	// A query is a read despite POST transport, so bounded retry is safe.
	r, err := doRead(ctx, n.API, n.Throttle, "POST", "databases/"+url.PathEscape(in.DatabaseID)+"/query", nil, body, n.headers(), 0)
	if err != nil {
		return NotionQueryOutput{}, err
	}
	var res struct {
		Results    []NotionPage `json:"results"`
		NextCursor string       `json:"next_cursor"`
		HasMore    bool         `json:"has_more"`
	}
	if jerr := json.Unmarshal(r.Body, &res); jerr != nil {
		return NotionQueryOutput{}, gws.NewError(gws.CodeProviderFailure, "notion database-query: unreadable provider body", nil)
	}
	if res.Results == nil {
		res.Results = []NotionPage{}
	}
	return NotionQueryOutput{SchemaVersion: SchemaVersion, Pages: res.Results,
		NextCursor: res.NextCursor, HasMore: res.HasMore, Binding: b}, nil
}
