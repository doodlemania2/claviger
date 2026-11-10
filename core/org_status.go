package core

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #84, 84-3 and 84-5): the `kind` label of an org and the
// counts-only status of an org. Nothing here reads or returns personal data.

// normalizeOrgKind trims a kind label and refuses control characters and
// labels longer than OrgKindMaxLength. The empty string means "no kind".
func normalizeOrgKind(kind string) (string, error) {
	kind = strings.TrimSpace(kind)

	if utf8.RuneCountInString(kind) > OrgKindMaxLength {
		return "", orgErr(400, "The kind can have at most 64 characters.")
	}

	for _, r := range kind {
		if unicode.IsControl(r) {
			return "", orgErr(400, "The kind cannot contain control characters.")
		}
	}

	return kind, nil
}

// OrgKinds returns the distinct non-empty kind labels in use, sorted. It is
// the source of the suggestions in the admin UI.
func OrgKinds(app App) []string {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if err != nil || col.Fields.GetByName("kind") == nil {
		return []string{}
	}

	kinds := []string{}

	err = app.DB().NewQuery("SELECT DISTINCT [[kind]] FROM {{" + CollectionNameOrgs +
		"}} WHERE [[kind]] IS NOT NULL AND [[kind]] != '' ORDER BY [[kind]]").Column(&kinds)
	if err != nil {
		return []string{}
	}

	return kinds
}

// OrgKindCount is one row of OrgStatusReport.DescendantsByKind. An empty Kind
// stands for orgs without a label.
type OrgKindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// OrgMemberCounts counts the memberships of one org.
type OrgMemberCounts struct {
	Active    int `json:"active"`
	Suspended int `json:"suspended"`
}

// OrgStatusReport is the counts-only status of an org.
type OrgStatusReport struct {
	// Level is the position of the org in its tree (a root is 1).
	Level int `json:"level"`
	// Height is how many levels sit below the org (0 for a leaf).
	Height int `json:"height"`
	// MaxDepth is the effective CLAVIGER_ORGS_MAX_DEPTH.
	MaxDepth int `json:"maxDepth"`
	// Descendants counts the live orgs below this one, ArchivedDescendants the
	// archived ones (not part of DescendantsByKind).
	Descendants         int             `json:"descendants"`
	ArchivedDescendants int             `json:"archivedDescendants"`
	DescendantsByKind   []OrgKindCount  `json:"descendantsByKind"`
	Members             OrgMemberCounts `json:"members"`
}

// OrgStatus computes the status of an org from the closure table.
func OrgStatus(app App, org *Record) (*OrgStatusReport, error) {
	report := &OrgStatusReport{MaxDepth: OrgMaxDepth(), DescendantsByKind: []OrgKindCount{}}

	up, err := orgAncestorClosure(app, org.Id)
	if err != nil {
		return nil, err
	}

	report.Level = max(len(up), 1)

	down, err := orgSubtreeClosure(app, org.Id)
	if err != nil {
		return nil, err
	}

	ids := []any{}

	for _, r := range down {
		report.Height = max(report.Height, r.Depth)

		if r.Descendant != org.Id {
			ids = append(ids, r.Descendant)
		}
	}

	if len(ids) > 0 {
		kids, err := app.FindAllRecords(CollectionNameOrgs, dbx.In("id", ids...))
		if err != nil {
			return nil, err
		}

		byKind := map[string]int{}

		for _, k := range kids {
			if k.GetBool("archived") {
				report.ArchivedDescendants++

				continue
			}

			report.Descendants++
			byKind[k.GetString("kind")]++
		}

		for kind, n := range byKind {
			report.DescendantsByKind = append(report.DescendantsByKind, OrgKindCount{Kind: kind, Count: n})
		}

		sort.Slice(report.DescendantsByKind, func(i, j int) bool {
			a, b := report.DescendantsByKind[i], report.DescendantsByKind[j]
			if a.Count != b.Count {
				return a.Count > b.Count
			}

			return a.Kind < b.Kind
		})
	}

	counts := []struct {
		Status string `db:"status"`
		N      int    `db:"n"`
	}{}

	err = app.DB().NewQuery("SELECT [[status]], COUNT(*) AS [[n]] FROM {{" + CollectionNameOrgMembers +
		"}} WHERE [[org]]={:org} GROUP BY [[status]]").Bind(dbx.Params{"org": org.Id}).All(&counts)
	if err != nil {
		return nil, err
	}

	for _, c := range counts {
		switch c.Status {
		case OrgStatusActive:
			report.Members.Active = c.N
		case OrgStatusSuspended:
			report.Members.Suspended = c.N
		}
	}

	return report, nil
}
