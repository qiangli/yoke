// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
)

// The pipe: an observation stream becomes a durable claim in the knowledge base.
//
// The dependency direction is deliberate and one-way. A stream may know about
// the store of record; the store of record must never know about a stream. kb
// is where knowledge lives regardless of who observed it, and the day it starts
// importing its feeders is the day adding a feeder means editing kb.
//
// What crosses the pipe is the CLAIM plus an ADDRESS — never the records. The
// page says "this fails here"; the address says where to look if you want to
// check. Copying the records in would put 30,000 lines a day into a wiki whose
// index has to stay readable, and would duplicate a corpus that already has a
// retention policy.

// promoteTag marks pages this pipe owns, so it can find its own work again
// without guessing from the title.
const promoteTag = "exec-observed"

// PromoteResult is what one pass did.
type PromoteResult struct {
	Written []string `json:"written"`
	Updated []string `json:"updated"`
	Skipped []string `json:"skipped"`
	Coverage
}

// PromoteToKB writes one candidate page per pitfall the evidence supports.
//
// Two properties matter more than the mechanics:
//
//  1. Every page is written as a CANDIDATE. Promotion is a candidate generator,
//     never an author. kb already has the ladder — `kb validate` requires evidence
//     to move a page to validated — and a recorder able to mint validated
//     knowledge would be the confabulation vector with a store attached.
//  2. It is idempotent by construction. The slug is derived from the command
//     template, so the same pitfall lands on the same page every run and a second
//     pass updates rather than appends. Blind appending is the documented death
//     spiral of agent memory, and a recorder runs far more often than a human.
func PromoteToKB(root string, store *kb.Store, opt PromoteOpts) (PromoteResult, error) {
	var res PromoteResult

	pitfalls, cov, err := Promote(root, opt)
	if err != nil {
		return res, err
	}
	res.Coverage = cov

	existing, err := store.List()
	if err != nil {
		return res, err
	}
	bySlug := map[string]*kb.Page{}
	for _, p := range existing {
		bySlug[p.Slug] = p
	}

	for _, pf := range pitfalls {
		page, slug := pageFor(pf)

		if prev, ok := bySlug[slug]; ok {
			// A human or another agent may have taken this page over —
			// validated it, superseded it, rewritten the body. Overwriting that
			// with a regenerated candidate would silently destroy a judgement
			// with a count, so the pipe declines and says so.
			if !ownedByPipe(prev) {
				res.Skipped = append(res.Skipped, slug)
				continue
			}
			// Slugs lose information: `go test ./hub/...` and `go test ./hub`
			// slugify identically. Updating one page from the other would merge
			// two distinct claims and report it as success, so a differing
			// template is a COLLISION and gets its own page rather than
			// overwriting.
			if was := TemplateOf(prev); was != "" && was != pf.Template {
				page.Slug = slug + "-" + shortHash(pf.Template)
				if err := store.Write(page, "add"); err != nil {
					return res, fmt.Errorf("kb write %s: %w", page.Slug, err)
				}
				res.Written = append(res.Written, page.Slug)
				continue
			}
			page.Created = prev.Created
			if err := store.Write(page, "update"); err != nil {
				return res, fmt.Errorf("kb write %s: %w", slug, err)
			}
			res.Updated = append(res.Updated, slug)
			continue
		}
		if err := store.Write(page, "add"); err != nil {
			return res, fmt.Errorf("kb write %s: %w", slug, err)
		}
		res.Written = append(res.Written, slug)
	}

	sort.Strings(res.Written)
	sort.Strings(res.Updated)
	sort.Strings(res.Skipped)
	return res, nil
}

// ownedByPipe reports a page this promotion may overwrite.
//
// Ownership is claimed by the tag AND by still being a candidate. The moment a
// human validates or supersedes one, it stops being ours: a person's judgement
// outranks a recount, and a pipe that overwrites it is worse than one that
// never ran.
func ownedByPipe(p *kb.Page) bool {
	if p.Status != kb.StatusCandidate {
		return false
	}
	for _, t := range p.Tags {
		if t == promoteTag {
			return true
		}
	}
	return false
}

// pageFor renders one pitfall as a knowledge page.
func pageFor(pf Pitfall) (*kb.Page, string) {
	title := pf.Template
	slug := kb.Slugify("exec " + title)

	ref := EvidenceRef{
		From: pf.FirstSeen, To: pf.LastSeen, N: pf.Failures,
	}

	page := &kb.Page{
		Slug: slug,
		// A pitfall is a gotcha: a thing that will bite you, not a lesson or a
		// runbook. The type is the routing surface, so getting it wrong sends
		// the page to the wrong readers.
		Type:  "gotcha",
		Title: title,
		// Description is what + WHEN, because it is what a reader matches on
		// before deciding to open the page at all.
		Description: describe(pf),
		Tags:        []string{promoteTag, pf.Cmd},
		Status:      kb.StatusCandidate,
		Evidence:    ref.String(),
		Scope:       &kb.Scope{OS: runtime.GOOS},
		Source:      &kb.Source{Tool: "bashy", Host: ""},
		Body:        body(pf, ref),
	}
	if pf.Dimension != "" {
		page.Tags = append(page.Tags, pf.Dimension)
	}
	return page, slug
}

func describe(pf Pitfall) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fails on %s", runtime.GOOS)
	if pf.Dimension != "" {
		fmt.Fprintf(&b, " (%s)", pf.Dimension)
	}
	fmt.Fprintf(&b, " — %d failures across %d sessions on %d days",
		pf.Failures, pf.Episodes, pf.Days)
	if !pf.LastSuccess.IsZero() {
		fmt.Fprintf(&b, "; last worked %s", pf.LastSuccess.Format("2006-01-02"))
	} else {
		b.WriteString("; never observed to work here")
	}
	return b.String()
}

// body states the claim and, immediately after it, everything needed to
// disagree with it.
//
// The counts are not decoration. A reader who can see 3 sessions over 2 days
// can judge the claim; one who sees only the verdict has to trust the
// threshold. That difference is the whole reason this is a candidate.
func body(pf Pitfall, ref EvidenceRef) string {
	var b strings.Builder

	fmt.Fprintf(&b, "`%s` has failed every time it was observed on this machine.\n\n", pf.Template)
	if pf.Dimension != "" {
		fmt.Fprintf(&b, "The advisor classified the failures as **%s**.\n\n", pf.Dimension)
	}

	b.WriteString("## Evidence\n\n")
	fmt.Fprintf(&b, "- %d failures across %d sessions on %d days\n",
		pf.Failures, pf.Episodes, pf.Days)
	if pf.ExitClass != "" && pf.ExitClass != "generic" {
		fmt.Fprintf(&b, "- exit: %s\n", pf.ExitClass)
	}
	fmt.Fprintf(&b, "- first %s, last %s\n",
		pf.FirstSeen.Format("2006-01-02"), pf.LastSeen.Format("2006-01-02"))
	if pf.Successes > 0 {
		fmt.Fprintf(&b, "- it HAS worked here %d time(s), most recently %s — so this is a regression, not a permanent property\n",
			pf.Successes, pf.LastSuccess.Format("2006-01-02"))
	} else {
		b.WriteString("- never observed to succeed here\n")
	}
	fmt.Fprintf(&b, "- records: `%s`\n\n", ref.String())

	b.WriteString("## Checking this\n\n")
	fmt.Fprintf(&b, "    bashy graph evidence '%s'\n\n", ref.String())
	b.WriteString("The records behind this page are a retained stream, not a copy held here. ")
	b.WriteString("Once they age out, the claim stays and the drill-down reports that its evidence was pruned.\n\n")

	b.WriteString("## Status\n\n")
	b.WriteString("Written by the execution recorder as a **candidate**. ")
	b.WriteString("It was counted, not judged. Validate it with `bashy kb validate` ")
	b.WriteString("once you have confirmed the cause, or supersede it with the fix.\n")

	// Machine-readable, so a later pass can tell whether two templates collided
	// onto one slug rather than assuming they are the same claim.
	fmt.Fprintf(&b, "\n<!-- exec-template: %s -->\n", pf.Template)
	return b.String()
}

// TemplateOf reads back the exact template a page was written for.
//
// Slugs lose information — `go test ./hub/...` and `go test ./hub` slugify the
// same — so the template is round-tripped in the body. Without it a second pass
// cannot tell an update from a collision, and would merge two claims into one
// while reporting success.
func TemplateOf(p *kb.Page) string {
	const open = "<!-- exec-template: "
	i := strings.Index(p.Body, open)
	if i < 0 {
		return ""
	}
	rest := p.Body[i+len(open):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// shortHash disambiguates two templates that slugify the same.
//
// Deterministic, so the collision page keeps its address across runs — a
// disambiguator that moved every pass would defeat the idempotence it exists to
// protect.
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}
