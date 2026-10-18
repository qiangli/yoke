package lenscmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/ref"
)

func init() {
	for _, scheme := range []string{"sprint", "run", "agent", "mb", "meet", "kb", "host", "story", "claim"} {
		if err := RegisterDeltaSource(scheme, &entityDeltaSource{scheme: scheme}); err != nil {
			panic(err)
		}
	}
}

// StoryResolver is an optional resolver for story locators.
// If unset, lookup checks AgentWorldRefs for ref.Todo.
var StoryResolver ref.Resolver

// ClaimResolver is an optional resolver for claim locators.
// If unset, claim delta resolves against the local coord ledger.
var ClaimResolver ref.Resolver

// RegisterEntityResolver registers an explicit ref.Resolver for an agent-world scheme.
// For story and claim, this sets StoryResolver and ClaimResolver.
// For other schemes, it registers on AgentWorldRefs.
func RegisterEntityResolver(scheme string, r ref.Resolver) {
	switch scheme {
	case "story":
		StoryResolver = r
	case "claim":
		ClaimResolver = r
	default:
		AgentWorldRefs.Register(ref.Kind(scheme), r)
	}
}

func lookupEntityResolver(scheme string) (ref.Resolver, bool) {
	switch scheme {
	case "story":
		if StoryResolver != nil {
			return StoryResolver, true
		}
		if res, ok := AgentWorldRefs.Lookup(ref.Todo); ok {
			return res, true
		}
		return nil, false
	case "claim":
		if ClaimResolver != nil {
			return ClaimResolver, true
		}
		return ref.ResolverFunc(resolveClaimFromStore), true
	default:
		return AgentWorldRefs.Lookup(ref.Kind(scheme))
	}
}

func resolveClaimFromStore(target string) (ref.Node, error) {
	dir := coord.DefaultDir()
	claims, err := coord.List(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ref.Node{}, fmt.Errorf("claim:%s: %w", target, ref.ErrNotFound)
		}
		return ref.Node{}, err
	}
	target = strings.TrimSpace(target)
	for _, c := range claims {
		if c.Resource == target || c.Project == target || c.Ref().Name == target || c.Ref().String() == target || fmt.Sprintf("%s:%s", c.Kind, c.Ref().Name) == target {
			n := ref.NewNode(ref.Kind("claim"), target)
			n.Title = c.Intent
			if n.Title == "" {
				n.Title = c.Project
			}
			n.Status = string(c.Liveness(time.Now()))
			n.Seq = int64(c.Epoch)
			n.UID = fmt.Sprintf("epoch:%d:rev:%d", c.Epoch, c.Rev)
			if len(c.Roots) > 0 {
				n.Where = strings.Join(c.Roots, ", ")
			}
			return n, nil
		}
	}
	return ref.Node{}, fmt.Errorf("claim:%s: %w", target, ref.ErrNotFound)
}

type entityDeltaSource struct {
	scheme string
}

func (e *entityDeltaSource) TranslateTime(ctx context.Context, loc ref.Locator, t time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.scheme == "run" {
		if res, ok := lookupEntityResolver("run"); ok {
			if r, ok := res.(ref.RunSourceResolver); ok {
				return (provider.RunHistory{Resolver: r}).TranslateTime(ctx, loc, t)
			}
		}
	}
	// ref.Node exposes identity and display fields, but no authoritative update time.
	return "", fmt.Errorf("provider %q does not support time/duration in --since: resolver has no authoritative update time", e.scheme)
}

func (e *entityDeltaSource) TranslateSeq(ctx context.Context, loc ref.Locator, seq int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("provider %q does not support sequence in --since", e.scheme)
}

func (e *entityDeltaSource) Delta(ctx context.Context, loc ref.Locator, token string) ([]byte, string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	if loc.Scheme != e.scheme {
		return nil, "", "", fmt.Errorf("lens: delta source for %s cannot serve %q", e.scheme, loc.Scheme)
	}
	if len(loc.Inner) != 0 {
		return nil, "", "", fmt.Errorf("lens: %s: container descent (!/) is not supported for records: %w", loc.Scheme, provider.ErrUnsupported)
	}
	if loc.ScopeKind != "" {
		return nil, "", "", fmt.Errorf("lens: %s: location scopes are not supported for records yet: %w", loc.Scheme, provider.ErrUnsupported)
	}
	target := strings.TrimSpace(loc.Target)
	if target == "" {
		return nil, "", "", fmt.Errorf("lens: %s: %w", loc.Scheme, ref.ErrEmptyID)
	}

	if e.scheme == "run" {
		if res, ok := lookupEntityResolver("run"); ok {
			if r, ok := res.(ref.RunSourceResolver); ok {
				return (provider.RunHistory{Resolver: r}).Delta(ctx, loc, token)
			}
		}
	}
	p := provider.NewEntity(AgentWorldRefs, e.scheme)
	res, ok := lookupEntityResolver(e.scheme)
	if !ok {
		return nil, "", "", fmt.Errorf("%w: %s", ref.ErrNoResolver, e.scheme)
	}
	p.Resolver = res
	node, err := p.Stat(ctx, loc)
	if err != nil {
		return nil, "", "", err
	}

	// A UID/Seq identifies an entity, not its revision. Hash the exact visible
	// record so title/status changes (and claim epoch/revision changes) replay.
	b, err := json.Marshal(node)
	if err != nil {
		return nil, "", "", err
	}
	nextToken := fmt.Sprintf("entity-v1:%x", sha256.Sum256(b))
	if token != "" {
		if !strings.HasPrefix(token, "entity-v1:") || len(token) != len(nextToken) {
			return nil, "", "", fmt.Errorf("invalid entity resume token; restart with --since=start")
		}
		if token == nextToken {
			return nil, nextToken, "", nil
		}
	}
	records := append(b, '\n')
	return records, nextToken, "", nil
}
