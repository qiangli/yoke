package apiadapters

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/external/saasclients"
)

// Slack is the typed data-action front for workspaces where the slack CLI
// (app tooling) does not reach: history, channel listing, message posting
// and deletion. Transport, auth, bounds and provider-error classification
// are the merged saasclients Slack Web API adapter (api.go); this file
// adds only typed versioned input/output and provider pagination on top.
// Message posting delegates to saasclients.SlackPost outright.
type Slack struct {
	API      saasclients.API
	Ledger   *saasclients.Ledger
	Throttle *Throttle
}

// NewSlack builds the adapter over an explicit endpoint (the Web API base
// in use, or a loopback fake in tests) and token. See saasclients.API:
// neither may be empty.
func NewSlack(baseURL, token string) *Slack {
	return &Slack{
		API:      saasclients.API{Client: "slack", BaseURL: baseURL, Token: token},
		Ledger:   &saasclients.Ledger{},
		Throttle: NewThrottle(350 * time.Millisecond),
	}
}

// SlackMessage is one conversations.history record, preserved verbatim
// where kept (Text is never reflowed; timestamps stay strings).
type SlackMessage struct {
	TS     string `json:"ts"`
	User   string `json:"user,omitempty"`
	Text   string `json:"text"`
	Thread string `json:"thread_ts,omitempty"`
}

// SlackChannel is one conversations.list record.
type SlackChannel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private,omitempty"`
	Members   int    `json:"num_members,omitempty"`
}

// SlackHistoryInput is the versioned history request. Cursor is the
// provider's opaque page cursor, verbatim; Limit caps one page.
type SlackHistoryInput struct {
	SchemaVersion string `json:"schema_version"`
	Channel       string `json:"channel"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}

// SlackHistoryOutput is the versioned history page. NextCursor is empty
// when the listing is complete (HasMore false).
type SlackHistoryOutput struct {
	SchemaVersion string         `json:"schema_version"`
	Messages      []SlackMessage `json:"messages"`
	NextCursor    string         `json:"next_cursor,omitempty"`
	HasMore       bool           `json:"has_more"`
	Binding       gws.Binding    `json:"-"`
}

// SlackChannelListInput is the versioned channel-listing request.
type SlackChannelListInput struct {
	SchemaVersion string `json:"schema_version"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}

// SlackChannelListOutput is the versioned channel-listing page.
type SlackChannelListOutput struct {
	SchemaVersion string         `json:"schema_version"`
	Channels      []SlackChannel `json:"channels"`
	NextCursor    string         `json:"next_cursor,omitempty"`
	HasMore       bool           `json:"has_more"`
	Binding       gws.Binding    `json:"-"`
}

// SlackPostInput is the versioned message-post intent.
type SlackPostInput struct {
	SchemaVersion string `json:"schema_version"`
	Channel       string `json:"channel"`
	Text          string `json:"text"`
	Key           string `json:"key,omitempty"`
	DryRun        bool   `json:"dry_run,omitempty"`
}

// SlackDeleteInput is the versioned message-delete intent (ts is the
// provider's natural id: a repeat converges on typed not-found).
type SlackDeleteInput struct {
	SchemaVersion string `json:"schema_version"`
	Channel       string `json:"channel"`
	TS            string `json:"ts"`
	Key           string `json:"key,omitempty"`
	DryRun        bool   `json:"dry_run,omitempty"`
}

func (in SlackHistoryInput) validate() *gws.Error {
	if err := CheckVersion(in.SchemaVersion, "history", "slack"); err != nil {
		return err
	}
	if in.Channel == "" {
		return gws.NewError(gws.CodeInvalidInput, "slack history: channel is required", nil)
	}
	if in.Limit < 0 || in.Limit > gws.MaxPageSize {
		return gws.NewError(gws.CodeOutputLimit,
			fmt.Sprintf("slack history: limit %d exceeds max page %d", in.Limit, gws.MaxPageSize), nil)
	}
	return nil
}

func (in SlackChannelListInput) validate() *gws.Error {
	if err := CheckVersion(in.SchemaVersion, "channel-list", "slack"); err != nil {
		return err
	}
	if in.Limit < 0 || in.Limit > gws.MaxPageSize {
		return gws.NewError(gws.CodeOutputLimit,
			fmt.Sprintf("slack channel-list: limit %d exceeds max page %d", in.Limit, gws.MaxPageSize), nil)
	}
	return nil
}

func pageLimit(limit int) string {
	if limit <= 0 {
		limit = gws.DefaultPageSize
	}
	return strconv.Itoa(limit)
}

// History reads one conversations.history page. Reads retry boundedly
// (doGet); the cursor travels verbatim and is binding-checked.
func (s *Slack) History(ctx context.Context, in SlackHistoryInput, b, issuedFor gws.Binding) (SlackHistoryOutput, *gws.Error) {
	if err := in.validate(); err != nil {
		return SlackHistoryOutput{}, err
	}
	if err := CheckCursor(b, issuedFor, in.Cursor); err != nil {
		return SlackHistoryOutput{}, err
	}
	r, err := doRead(ctx, s.API, s.Throttle, "GET", "conversations.history",
		map[string]string{"channel": in.Channel, "limit": pageLimit(in.Limit), "cursor": in.Cursor}, nil, nil, 0)
	if err != nil {
		return SlackHistoryOutput{}, err
	}
	var res struct {
		Messages []SlackMessage `json:"messages"`
		HasMore  bool           `json:"has_more"`
		Meta     struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if jerr := json.Unmarshal(r.Body, &res); jerr != nil {
		return SlackHistoryOutput{}, gws.NewError(gws.CodeProviderFailure, "slack history: unreadable provider body", nil)
	}
	if res.Messages == nil {
		res.Messages = []SlackMessage{}
	}
	return SlackHistoryOutput{SchemaVersion: SchemaVersion, Messages: res.Messages,
		NextCursor: res.Meta.NextCursor, HasMore: res.HasMore, Binding: b}, nil
}

// ChannelList reads one conversations.list page.
func (s *Slack) ChannelList(ctx context.Context, in SlackChannelListInput, b, issuedFor gws.Binding) (SlackChannelListOutput, *gws.Error) {
	if err := in.validate(); err != nil {
		return SlackChannelListOutput{}, err
	}
	if err := CheckCursor(b, issuedFor, in.Cursor); err != nil {
		return SlackChannelListOutput{}, err
	}
	r, err := doRead(ctx, s.API, s.Throttle, "GET", "conversations.list",
		map[string]string{"limit": pageLimit(in.Limit), "cursor": in.Cursor}, nil, nil, 0)
	if err != nil {
		return SlackChannelListOutput{}, err
	}
	var res struct {
		Channels []SlackChannel `json:"channels"`
		Meta     struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if jerr := json.Unmarshal(r.Body, &res); jerr != nil {
		return SlackChannelListOutput{}, gws.NewError(gws.CodeProviderFailure, "slack channel-list: unreadable provider body", nil)
	}
	if res.Channels == nil {
		res.Channels = []SlackChannel{}
	}
	out := SlackChannelListOutput{SchemaVersion: SchemaVersion, Channels: res.Channels,
		NextCursor: res.Meta.NextCursor, HasMore: res.Meta.NextCursor != "", Binding: b}
	return out, nil
}

// Post validates input and delegates to the merged saasclients.SlackPost:
// sandbox-gated, ledger-guarded, dry-run safe. It is the same mutation the
// business story verified; this wrapper only types its input.
func (s *Slack) Post(ctx context.Context, scope saasclients.Scope, in SlackPostInput) (string, *gws.Error) {
	if err := CheckVersion(in.SchemaVersion, "message-post", "slack"); err != nil {
		return "", err
	}
	if in.Channel == "" || in.Text == "" {
		return "", gws.NewError(gws.CodeInvalidInput, "slack message-post: channel and text are required", nil)
	}
	payload, _ := json.Marshal(map[string]string{"channel": in.Channel, "text": in.Text})
	m := saasclients.Mutation{Scope: scope, Op: "message-post", Key: in.Key, Payload: payload, DryRun: in.DryRun}
	return saasclients.SlackPost(ctx, s.API, s.Ledger, m, in.Channel, in.Text)
}

// Delete removes one message by ts. The mutation validates before effects
// (admitted op, bounded payload) and never retries; a repeat converges on
// typed not-found.
func (s *Slack) Delete(ctx context.Context, scope saasclients.Scope, in SlackDeleteInput) *gws.Error {
	if err := CheckVersion(in.SchemaVersion, "message-delete", "slack"); err != nil {
		return err
	}
	if in.Channel == "" || in.TS == "" {
		return gws.NewError(gws.CodeInvalidInput, "slack message-delete: channel and ts are required", nil)
	}
	payload, _ := json.Marshal(map[string]string{"channel": in.Channel, "ts": in.TS})
	m := saasclients.Mutation{Scope: scope, Op: "message-delete", Key: in.Key, Payload: payload, DryRun: in.DryRun}
	if _, err := m.Validate(); err != nil {
		return err
	}
	if in.DryRun {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"channel": in.Channel, "ts": in.TS})
	_, err := doMutate(ctx, s.API, s.Throttle, "POST", "chat.delete", body, nil)
	return err
}
