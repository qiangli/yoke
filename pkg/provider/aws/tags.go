// Tag discipline for the AWS adapter (S522.2).
//
// Two tag families ride on every resource this adapter creates:
//
//   - the contract's bashy-lease tags (provider.LeaseTags): the lease
//     marker, the owner, the hashed idempotency key and, for a temporary
//     authorization, the temporary marker plus its deadline/work tag. They
//     are what makes a replayed create find its instance instead of
//     allocating a second one, and what stops the adapter from touching a
//     foreign instance.
//   - the Sprint 522 dhnt tags: dhnt:managed=true always, plus
//     dhnt:sprint / dhnt:story / dhnt:ttl when the caller supplies them.
//     The automation IAM policy only permits deleting resources tagged
//     dhnt:managed=true, so destroy refuses up front on an instance that
//     lacks it rather than failing as an opaque AccessDeniedException.
//
// Lightsail tags are key/value pairs; the contract's tags are flat
// strings. The mapping is lossless and total: a string with no "=" becomes
// a key with no value, a "k=v" string becomes that pair, and reading them
// back rebuilds the exact same strings. That is what lets provider.Owned
// run unchanged against a Lightsail instance.
package aws

import (
	"slices"
	"strings"
)

// dhnt tag keys. dhnt:managed is the one the IAM delete policy keys on.
const (
	TagManagedKey   = "dhnt:managed"
	TagManagedValue = "true"
	TagSprintKey    = "dhnt:sprint"
	TagStoryKey     = "dhnt:story"
	TagTTLKey       = "dhnt:ttl"
	// TagUserDataKey records the S3 object key of a staged first-boot
	// script (s3.go). It is remembered as a TAG because the automation
	// IAM policy grants no s3:ListBucket: destroy cannot discover the
	// object, so create has to say where it put it. The bucket's
	// one-day lifecycle rule is the backstop for an instance whose tag
	// is gone.
	TagUserDataKey = "dhnt:userdata-key"
)

// ManagedTag is the flat-string form of the managed marker.
const ManagedTag = TagManagedKey + "=" + TagManagedValue

// encodeTags converts flat contract tag strings to Lightsail tags,
// dropping empties and keeping the first occurrence of a key.
func encodeTags(flat []string) []Tag {
	out := make([]Tag, 0, len(flat))
	seen := map[string]bool{}
	for _, s := range flat {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, Tag{Key: k, Value: v})
	}
	return out
}

// decodeTags converts Lightsail tags back to flat contract tag strings.
func decodeTags(tags []Tag) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t.Key == "" {
			continue
		}
		if t.Value == "" {
			out = append(out, t.Key)
			continue
		}
		out = append(out, t.Key+"="+t.Value)
	}
	return out
}

// Managed reports whether tags carry dhnt:managed=true, the marker the
// automation IAM policy requires before it will permit a delete.
func Managed(flat []string) bool {
	return slices.Contains(flat, ManagedTag)
}

// UserDataKey returns the staged-script object key recorded on a
// resource, or "" when its user data went inline.
func UserDataKey(flat []string) string {
	for _, t := range flat {
		if k, v, found := strings.Cut(t, "="); found && k == TagUserDataKey {
			return v
		}
	}
	return ""
}

// dhntTags are the Sprint 522 governance tags for one create.
func (a *adapter) dhntTags() []string {
	out := []string{ManagedTag}
	for _, kv := range [][2]string{
		{TagSprintKey, a.cfg.Sprint},
		{TagStoryKey, a.cfg.Story},
		{TagTTLKey, a.cfg.TTL},
	} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			out = append(out, kv[0]+"="+v)
		}
	}
	return out
}
