package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Well-known tags for cloudbox ownership and soft-delete lifecycle.
const (
	TagLease             = "bashy-lease"
	TagOwnerPrefix       = "bashy-owner-"
	TagTrash             = "bashy-trash"
	TagPermanent         = "bashy-permanent"
	TagPurgeAfterPrefix  = "bashy-purge-after-"
	TagTrashReasonPrefix = "bashy-trash-reason-"
)

// ValidateCloudboxOwnership checks that a droplet has the required cloudbox
// ownership tags (bashy-lease and a bashy-owner-* tag) and is not tagged
// permanent (bashy-permanent).
func ValidateCloudboxOwnership(tags []string) error {
	hasLease := slices.Contains(tags, TagLease)
	var hasOwner bool
	for _, t := range tags {
		if strings.HasPrefix(t, TagOwnerPrefix) && len(t) > len(TagOwnerPrefix) {
			hasOwner = true
			break
		}
	}
	if !hasLease && !hasOwner {
		return errors.New("missing cloudbox ownership tags (bashy-lease and bashy-owner-*)")
	}
	if !hasLease {
		return errors.New("missing cloudbox lease tag (bashy-lease)")
	}
	if !hasOwner {
		return errors.New("missing cloudbox owner tag (bashy-owner-*)")
	}
	if slices.Contains(tags, TagPermanent) {
		return errors.New("droplet is tagged permanent (bashy-permanent)")
	}
	return nil
}

// ReasonSlug formats a free-text reason into a safe slug suitable for DO tags.
func ReasonSlug(reason string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(reason)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 64 {
		slug = strings.Trim(slug[:64], "-")
	}
	return slug
}

// ParsePurgeAfter extracts the latest bashy-purge-after-<unix> timestamp from tags.
func ParsePurgeAfter(tags []string) (int64, bool) {
	var best int64
	var found bool
	for _, t := range tags {
		if s, ok := strings.CutPrefix(t, TagPurgeAfterPrefix); ok {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				if n > best {
					best = n
				}
				found = true
			}
		}
	}
	return best, found
}

// ParseTrashReason extracts the reason slug from bashy-trash-reason-<slug>.
func ParseTrashReason(tags []string) (string, bool) {
	for _, t := range tags {
		if s, ok := strings.CutPrefix(t, TagTrashReasonPrefix); ok {
			return s, true
		}
	}
	return "", false
}

// dropletAction issues an action on a droplet and waits for it to complete.
func (c *Client) dropletAction(ctx context.Context, id int, actionType string) error {
	body := map[string]string{"type": actionType}
	var out struct {
		Action struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := c.call(ctx, http.MethodPost, fmt.Sprintf("/v2/droplets/%d/actions", id), body, &out); err != nil {
		return err
	}
	return c.waitAction(ctx, out.Action.ID, out.Action.Status)
}

// PowerOffDroplet powers off a droplet if it is not already off. Idempotent.
func (c *Client) PowerOffDroplet(ctx context.Context, id int) error {
	d, err := c.GetDroplet(ctx, id)
	if err != nil {
		return err
	}
	if d.Status == "off" {
		return nil
	}
	return c.dropletAction(ctx, id, "power_off")
}

// PowerOnDroplet powers on a droplet if it is not already active. Idempotent.
func (c *Client) PowerOnDroplet(ctx context.Context, id int) error {
	d, err := c.GetDroplet(ctx, id)
	if err != nil {
		return err
	}
	if d.Status == "active" {
		return nil
	}
	return c.dropletAction(ctx, id, "power_on")
}

// ListDropletsByTag lists droplets matching tag, handling pagination.
func (c *Client) ListDropletsByTag(ctx context.Context, tag string) ([]DropletDetail, error) {
	var all []DropletDetail
	path := "/v2/droplets?per_page=200&tag_name=" + url.QueryEscape(tag)
	for page := 0; path != "" && page < 1000; page++ {
		var out struct {
			Droplets []DropletDetail `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Droplets...)
		path = ""
		if next := out.Links.Pages.Next; next != "" {
			u, err := url.Parse(next)
			if err != nil {
				return nil, fmt.Errorf("digitalocean: bad pagination link: %w", err)
			}
			path = u.EscapedPath() + "?" + u.RawQuery
		}
	}
	return all, nil
}

// TrashOptions specifies parameters for trashing a droplet.
type TrashOptions struct {
	DropletID     int
	Grace         time.Duration
	GraceExplicit bool
	Reason        string
	DryRun        bool
	Now           time.Time
}

// TrashResult records the outcome of a trash operation.
type TrashResult struct {
	DropletID   int      `json:"droplet_id"`
	DryRun      bool     `json:"dry_run"`
	Status      string   `json:"status"` // "trashed", "planned", "noop"
	PurgeAfter  int64    `json:"purge_after"`
	Reason      string   `json:"reason,omitempty"`
	TagsAdded   []string `json:"tags_added,omitempty"`
	TagsRemoved []string `json:"tags_removed,omitempty"`
	PoweredOff  bool     `json:"powered_off"`
	Message     string   `json:"message,omitempty"`
}

// TrashDroplet soft-deletes a cloudbox-owned droplet by adding bashy-trash,
// bashy-purge-after-<unix>, and optional bashy-trash-reason-<slug> tags, then
// powering it off. It is idempotent and refuses foreign or permanent droplets.
func (c *Client) TrashDroplet(ctx context.Context, opts TrashOptions) (*TrashResult, error) {
	if opts.DropletID <= 0 {
		return nil, errors.New("droplet ID is required")
	}
	d, err := c.GetDroplet(ctx, opts.DropletID)
	if err != nil {
		return nil, fmt.Errorf("droplet %d: %w", opts.DropletID, err)
	}

	if err := ValidateCloudboxOwnership(d.Tags); err != nil {
		return nil, fmt.Errorf("refuse: droplet %d: %w", opts.DropletID, err)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	alreadyTrashed := slices.Contains(d.Tags, TagTrash)
	existingPurge, hasPurge := ParsePurgeAfter(d.Tags)

	grace := opts.Grace
	if grace <= 0 {
		grace = 24 * time.Hour
	}

	var purgeAfter int64
	if alreadyTrashed && hasPurge && !opts.GraceExplicit {
		purgeAfter = existingPurge
	} else {
		purgeAfter = now.Add(grace).Unix()
	}

	purgeTag := fmt.Sprintf("%s%d", TagPurgeAfterPrefix, purgeAfter)

	slug := ReasonSlug(opts.Reason)
	var reasonTag string
	if slug != "" {
		reasonTag = TagTrashReasonPrefix + slug
	} else if alreadyTrashed {
		if existingReason, hasReason := ParseTrashReason(d.Tags); hasReason {
			slug = existingReason
			reasonTag = TagTrashReasonPrefix + slug
		}
	}

	var tagsToAdd []string
	if !slices.Contains(d.Tags, TagTrash) {
		tagsToAdd = append(tagsToAdd, TagTrash)
	}
	if !slices.Contains(d.Tags, purgeTag) {
		tagsToAdd = append(tagsToAdd, purgeTag)
	}
	if reasonTag != "" && !slices.Contains(d.Tags, reasonTag) {
		tagsToAdd = append(tagsToAdd, reasonTag)
	}

	var tagsToRemove []string
	for _, t := range d.Tags {
		if strings.HasPrefix(t, TagPurgeAfterPrefix) && t != purgeTag {
			tagsToRemove = append(tagsToRemove, t)
		}
		if reasonTag != "" && strings.HasPrefix(t, TagTrashReasonPrefix) && t != reasonTag {
			tagsToRemove = append(tagsToRemove, t)
		}
	}

	powerOffNeeded := d.Status != "off"

	res := &TrashResult{
		DropletID:   opts.DropletID,
		DryRun:      opts.DryRun,
		PurgeAfter:  purgeAfter,
		Reason:      slug,
		TagsAdded:   tagsToAdd,
		TagsRemoved: tagsToRemove,
		PoweredOff:  powerOffNeeded,
	}

	if opts.DryRun {
		res.Status = "planned"
		return res, nil
	}

	for _, t := range tagsToRemove {
		if err := c.UntagDroplet(ctx, t, opts.DropletID); err != nil {
			return nil, fmt.Errorf("untag %q: %w", t, err)
		}
	}
	for _, t := range tagsToAdd {
		if err := c.TagDroplet(ctx, t, opts.DropletID); err != nil {
			return nil, fmt.Errorf("tag %q: %w", t, err)
		}
	}
	if powerOffNeeded {
		if err := c.dropletAction(ctx, opts.DropletID, "power_off"); err != nil {
			return nil, fmt.Errorf("power off droplet %d: %w", opts.DropletID, err)
		}
	}

	if len(tagsToAdd) == 0 && len(tagsToRemove) == 0 && !powerOffNeeded {
		res.Status = "noop"
	} else {
		res.Status = "trashed"
	}
	return res, nil
}

// RestoreOptions specifies parameters for restoring a trashed droplet.
type RestoreOptions struct {
	DropletID int
	DryRun    bool
}

// RestoreResult records the outcome of a restore operation.
type RestoreResult struct {
	DropletID   int      `json:"droplet_id"`
	DryRun      bool     `json:"dry_run"`
	Status      string   `json:"status"` // "restored", "planned", "noop"
	TagsRemoved []string `json:"tags_removed,omitempty"`
	PoweredOn   bool     `json:"powered_on"`
	Message     string   `json:"message,omitempty"`
}

// RestoreDroplet restores a trashed droplet by removing bashy-trash,
// bashy-purge-after-*, and bashy-trash-reason-* tags, then powering it on.
func (c *Client) RestoreDroplet(ctx context.Context, opts RestoreOptions) (*RestoreResult, error) {
	if opts.DropletID <= 0 {
		return nil, errors.New("droplet ID is required")
	}
	d, err := c.GetDroplet(ctx, opts.DropletID)
	if err != nil {
		return nil, fmt.Errorf("droplet %d: %w", opts.DropletID, err)
	}

	if err := ValidateCloudboxOwnership(d.Tags); err != nil {
		return nil, fmt.Errorf("refuse: droplet %d: %w", opts.DropletID, err)
	}

	var tagsToRemove []string
	for _, t := range d.Tags {
		if t == TagTrash || strings.HasPrefix(t, TagPurgeAfterPrefix) || strings.HasPrefix(t, TagTrashReasonPrefix) {
			tagsToRemove = append(tagsToRemove, t)
		}
	}

	powerOnNeeded := d.Status != "active"

	res := &RestoreResult{
		DropletID:   opts.DropletID,
		DryRun:      opts.DryRun,
		TagsRemoved: tagsToRemove,
		PoweredOn:   powerOnNeeded,
	}

	if opts.DryRun {
		res.Status = "planned"
		return res, nil
	}

	for _, t := range tagsToRemove {
		if err := c.UntagDroplet(ctx, t, opts.DropletID); err != nil {
			return nil, fmt.Errorf("untag %q: %w", t, err)
		}
	}
	if powerOnNeeded {
		if err := c.dropletAction(ctx, opts.DropletID, "power_on"); err != nil {
			return nil, fmt.Errorf("power on droplet %d: %w", opts.DropletID, err)
		}
	}

	if len(tagsToRemove) == 0 && !powerOnNeeded {
		res.Status = "noop"
	} else {
		res.Status = "restored"
	}
	return res, nil
}

// TrashedDroplet contains the summary of a droplet currently in trash.
type TrashedDroplet struct {
	DropletID         int      `json:"droplet_id"`
	Name              string   `json:"name"`
	Status            string   `json:"status"`
	PurgeAfter        int64    `json:"purge_after"`
	PurgeAfterRFC3339 string   `json:"purge_after_rfc3339,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	Tags              []string `json:"tags,omitempty"`
}

// ListTrashedDroplets returns all droplets tagged with bashy-trash, sorted by droplet ID.
func (c *Client) ListTrashedDroplets(ctx context.Context) ([]TrashedDroplet, error) {
	droplets, err := c.ListDropletsByTag(ctx, TagTrash)
	if err != nil {
		return nil, err
	}
	var out []TrashedDroplet
	for _, d := range droplets {
		purgeAfter, _ := ParsePurgeAfter(d.Tags)
		reason, _ := ParseTrashReason(d.Tags)
		var rfc3339 string
		if purgeAfter > 0 {
			rfc3339 = time.Unix(purgeAfter, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, TrashedDroplet{
			DropletID:         d.ID,
			Name:              d.Name,
			Status:            d.Status,
			PurgeAfter:        purgeAfter,
			PurgeAfterRFC3339: rfc3339,
			Reason:            reason,
			Tags:              slices.Clone(d.Tags),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DropletID < out[j].DropletID })
	if out == nil {
		out = []TrashedDroplet{}
	}
	return out, nil
}
