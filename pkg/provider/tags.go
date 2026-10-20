package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The bashy-lease tag discipline: the tags on a provider resource are the
// durable record of who owns it, which request created it and whether it
// was created as temporary. They survive the controller (a fresh process,
// or a lost create response, can recover everything from the provider), so
// adapters must write them at create and read them back before acting.
//
// Every tag fits the common provider tag charset [A-Za-z0-9_:-], max 255.
const (
	// TagLease marks a resource as created through this contract.
	TagLease = "bashy-lease"
	// TagTemporary marks a resource created with a temporary-lifecycle
	// authorization (A6); only such a resource may be destroyed under one.
	TagTemporary = "bashy-temp"

	tagOwnerPrefix    = "bashy-owner-"
	tagIdemPrefix     = "bashy-idem-"
	tagDeadlinePrefix = "bashy-deadline-"
	tagWorkPrefix     = "bashy-work-"
)

var tagSafe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidOwner reports whether owner can be used verbatim in OwnerTag.
func ValidOwner(owner string) bool { return tagSafe.MatchString(owner) }

// OwnerTag is the ownership tag for an owner; an adapter acts on an
// existing resource only when it carries TagLease and its owner's tag.
// owner must satisfy ValidOwner.
func OwnerTag(owner string) string { return tagOwnerPrefix + owner }

// IdempotencyTag is the tag recording the idempotency key of the create
// that made a resource. Keys are hashed, so any key string is safe.
func IdempotencyTag(key string) string { return tagIdemPrefix + digest(key) }

// DeadlineTag records a TTL. When several are present the latest wins
// (DeadlineFromTags), which is how a lease is extended.
func DeadlineTag(t time.Time) string { return tagDeadlinePrefix + strconv.FormatInt(t.Unix(), 10) }

// WorkTag records the work a work-bound temporary resource belongs to.
// A ref that is not tag-safe is hashed.
func WorkTag(ref string) string {
	if tagSafe.MatchString(ref) {
		return tagWorkPrefix + ref
	}
	return tagWorkPrefix + digest(ref)
}

// LeaseTags returns the tags a create must write: lease, owner,
// idempotency key and, for a temporary authorization, the temporary
// marker plus its deadline and/or work tag.
func LeaseTags(owner, idempotencyKey string, temp *Authorization) []string {
	tags := []string{TagLease, OwnerTag(owner), IdempotencyTag(idempotencyKey)}
	if temp != nil && temp.Kind == AuthTemporary {
		tags = append(tags, TagTemporary)
		if !temp.ExpiresAt.IsZero() {
			tags = append(tags, DeadlineTag(temp.ExpiresAt))
		}
		if temp.WorkRef != "" {
			tags = append(tags, WorkTag(temp.WorkRef))
		}
	}
	return tags
}

// Owned reports whether tags carry the lease and owner's tags.
func Owned(tags []string, owner string) bool {
	var lease, own bool
	for _, t := range tags {
		lease = lease || t == TagLease
		own = own || t == OwnerTag(owner)
	}
	return lease && own
}

// DeadlineFromTags returns the latest recorded deadline, if any.
func DeadlineFromTags(tags []string) (time.Time, bool) {
	var best int64
	for _, t := range tags {
		if v, ok := strings.CutPrefix(t, tagDeadlinePrefix); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > best {
				best = n
			}
		}
	}
	if best == 0 {
		return time.Time{}, false
	}
	return time.Unix(best, 0).UTC(), true
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
