package core

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/filesystem"
)

// Fork-local (Claviger #80, 80-4): merge and split households, the duplicate
// candidate list, member photos and the repair of the `household` copies.
//
// Merge and split move member rows with plain UPDATE statements inside one
// transaction (like the graduation job): the per-row audit entries of the
// record hooks would bury the one entry that matters, and the guards are
// checked here for the whole result instead of row by row. Each operation
// writes exactly one audit entry (household_merge / household_split) that
// holds ids only: no names, no birth dates, nothing encrypted.

const (
	AuditActionHouseholdMerge = "household_merge"
	AuditActionHouseholdSplit = "household_split"

	// HouseholdDuplicatesLimit caps the candidate list.
	HouseholdDuplicatesLimit = 200

	householdIdChunk = 400
)

// ---- dependents ---------------------------------------------------------------

// HouseholdDependent is a collection that describes a person: a relation to
// _householdMembers (Member) and a copy of the household (Household), the
// shape BindHouseholdCopy keeps in step.
type HouseholdDependent struct {
	Collection string
	Member     string
	Household  string
}

// HouseholdDependents finds the collections that carry both a single relation
// to _householdMembers and a single relation to _households. Fields named
// "member" and "household" win; otherwise a collection with exactly one of
// each qualifies. Anything ambiguous is skipped (list it with the repair
// command's report, not guessed at).
func HouseholdDependents(app App) ([]HouseholdDependent, error) {
	mcol, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers)
	if err != nil {
		return nil, err
	}

	hcol, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholds)
	if err != nil {
		return nil, err
	}

	cols, err := app.FindAllCollections(CollectionTypeBase, CollectionTypeAuth)
	if err != nil {
		return nil, err
	}

	var out []HouseholdDependent

	for _, c := range cols {
		if c.Id == mcol.Id || c.Id == hcol.Id {
			continue
		}

		var members, households []string

		for _, f := range c.Fields {
			rf, ok := f.(*RelationField)
			if !ok || rf.IsMultiple() {
				continue
			}

			switch rf.CollectionId {
			case mcol.Id:
				members = append(members, rf.Name)
			case hcol.Id:
				households = append(households, rf.Name)
			}
		}

		pick := func(names []string, want string) string {
			if slices.Contains(names, want) {
				return want
			}

			if len(names) == 1 {
				return names[0]
			}

			return ""
		}

		m, h := pick(members, "member"), pick(households, "household")
		if m != "" && h != "" {
			out = append(out, HouseholdDependent{Collection: c.Name, Member: m, Household: h})
		}
	}

	return out, nil
}

func chunkIds(ids []string) [][]string {
	var out [][]string

	for len(ids) > 0 {
		n := min(householdIdChunk, len(ids))
		out = append(out, ids[:n])
		ids = ids[n:]
	}

	return out
}

func idArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}

	return out
}

// moveHouseholdMembers re-homes the rows and rewrites the household copy of
// every dependent record about them. It returns the number of copies rewritten.
func moveHouseholdMembers(txApp App, ids []string, to *Record) (int, error) {
	deps, err := HouseholdDependents(txApp)
	if err != nil {
		return 0, err
	}

	stamp := credentialDateStamp(householdNow())
	copies := 0

	for _, chunk := range chunkIds(ids) {
		if _, err := txApp.NonconcurrentDB().Update(CollectionNameHouseholdMembers,
			dbx.Params{"household": to.Id, "updated": stamp}, dbx.In("id", idArgs(chunk)...)).Execute(); err != nil {
			return 0, err
		}

		for _, d := range deps {
			res, err := txApp.NonconcurrentDB().Update(d.Collection,
				dbx.Params{d.Household: to.Id}, dbx.In(d.Member, idArgs(chunk)...)).Execute()
			if err != nil {
				return 0, err
			}

			n, _ := res.RowsAffected()
			copies += int(n)
		}
	}

	return copies, nil
}

func memberIds(rows []*Record) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Id)
	}

	return out
}

func writeHouseholdMoveAudit(app App, ctx context.Context, action string, target *Record, org *Record, meta map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	meta["orgId"] = org.Id

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   target.Collection().Id,
		CollectionName: CollectionNameHouseholds,
		RecordId:       target.Id,
		ChangedFields:  []string{"members"},
		Meta:           meta,
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, target)
}

// ---- merge --------------------------------------------------------------------

// MergeHouseholds moves every member row of the source household into target,
// rewrites the household copy of the dependent records, archives the source and
// writes one audit entry. Staff of the target's org only. Duplicates are left
// for staff to handle. The merge is refused when the result would break a
// guard (an active minor with no active adult guardian) or when one login sits
// in both households.
func MergeHouseholds(app App, ctx context.Context, actor *HouseholdActor, org, target *Record, fromId string) error {
	if !actor.Staff {
		return householdErr(403, "Only the staff of the organization can merge households.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return err
	}

	if fromId == "" {
		return householdErr(400, "The source household is required.")
	}

	if fromId == target.Id {
		return householdErr(400, "A household cannot be merged into itself.")
	}

	src, _, err := FindHousehold(app, fromId)
	if err != nil || src.GetString("org") != org.Id {
		return householdErr(404, "Source household not found.")
	}

	return app.RunInTransaction(func(txApp App) error {
		// reload inside the transaction: the checks and the move see one state
		tgt, err := txApp.FindRecordById(CollectionNameHouseholds, target.Id)
		if err != nil {
			return householdErr(404, "Household not found.")
		}

		src, err := txApp.FindRecordById(CollectionNameHouseholds, src.Id)
		if err != nil {
			return householdErr(404, "Source household not found.")
		}

		if tgt.GetString("status") != HouseholdStatusActive || src.GetString("status") != HouseholdStatusActive {
			return householdErr(409, "Both households must be active.")
		}

		tm, err := HouseholdMembers(txApp, tgt)
		if err != nil {
			return err
		}

		sm, err := HouseholdMembers(txApp, src)
		if err != nil {
			return err
		}

		logins := map[string]bool{}

		for _, m := range tm {
			if u := m.GetString("user"); u != "" {
				logins[u] = true
			}
		}

		for _, m := range sm {
			if u := m.GetString("user"); u != "" && logins[u] {
				return householdErr(409, "The same login is in both households: remove one of the two people first.")
			}
		}

		if householdNeedsGuardian(slices.Concat(tm, sm)) {
			return householdErr(409, "A household with a minor needs at least one active adult guardian.")
		}

		ids := memberIds(sm)

		copies, err := moveHouseholdMembers(txApp, ids, tgt)
		if err != nil {
			return err
		}

		// a plain UPDATE: the archive is part of the merge entry, not an entry of its own
		if _, err := txApp.NonconcurrentDB().Update(CollectionNameHouseholds,
			dbx.Params{"status": HouseholdStatusArchived, "updated": credentialDateStamp(householdNow())},
			dbx.HashExp{"id": src.Id}).Execute(); err != nil {
			return err
		}

		return writeHouseholdMoveAudit(txApp, ctx, AuditActionHouseholdMerge, tgt, org, map[string]any{
			"mergedFrom":      src.Id,
			"movedMembers":    ids,
			"copiesRewritten": copies,
		})
	})
}

// ---- split --------------------------------------------------------------------

// SplitHousehold moves the listed members of h into a new active household of
// the same org. Staff, or an active head of h, may do it. The guards hold for
// both resulting households. The new household starts without address and
// phone (they are sensitive data a person re-enters on purpose). The audit
// entry lists the ids moved, so a merge can undo it.
func SplitHousehold(app App, ctx context.Context, actor *HouseholdActor, org, h *Record, ids []string, name string) (*Record, *Record, error) {
	head := actor.Member != nil && actor.Member.GetString("role") == HouseholdRoleHead && !actor.Member.GetBool("isMinor")
	if !actor.Staff && !head {
		return nil, nil, householdErr(403, "Only the staff of the organization or a head of the household can split it.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, nil, err
	}

	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, nil, householdErr(400, "A name of up to 200 characters is required.")
	}

	var unique []string

	for _, id := range ids {
		if id != "" && !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}

	if len(unique) == 0 {
		return nil, nil, householdErr(400, "Name at least one person to move.")
	}

	var from, to *Record

	err := app.RunInTransaction(func(txApp App) error {
		src, err := txApp.FindRecordById(CollectionNameHouseholds, h.Id)
		if err != nil {
			return householdErr(404, "Household not found.")
		}

		if src.GetString("status") != HouseholdStatusActive {
			return householdErr(409, "The household is archived.")
		}

		all, err := HouseholdMembers(txApp, src)
		if err != nil {
			return err
		}

		var moved, stay []*Record

		for _, m := range all {
			if slices.Contains(unique, m.Id) {
				moved = append(moved, m)
			} else {
				stay = append(stay, m)
			}
		}

		if len(moved) != len(unique) {
			return householdErr(400, "Every person to move must belong to the household.")
		}

		if len(stay) == 0 {
			return householdErr(400, "Someone must stay in the household: to move everyone, rename it instead.")
		}

		if householdNeedsGuardian(moved) || householdNeedsGuardian(stay) {
			return householdErr(409, "A household with a minor needs at least one active adult guardian.")
		}

		col, err := txApp.FindCachedCollectionByNameOrId(CollectionNameHouseholds)
		if err != nil {
			return err
		}

		// no record hooks: the one audit entry below describes the whole split
		rec := NewRecord(col)
		rec.Id = GenerateDefaultRandomId()
		rec.Set("org", org.Id)
		rec.Set("name", name)
		rec.Set("status", HouseholdStatusActive)

		if err := txApp.UnsafeWithoutHooks().Save(rec); err != nil {
			return wrapOrgSaveErr(err)
		}

		movedIds := memberIds(moved)

		copies, err := moveHouseholdMembers(txApp, movedIds, rec)
		if err != nil {
			return err
		}

		if err := writeHouseholdMoveAudit(txApp, ctx, AuditActionHouseholdSplit, rec, org, map[string]any{
			"splitFrom":       src.Id,
			"movedMembers":    movedIds,
			"copiesRewritten": copies,
		}); err != nil {
			return err
		}

		from, to = src, rec

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return from, to, nil
}

// ---- duplicates ---------------------------------------------------------------

// HouseholdDuplicateMember is one person of a candidate group. It carries no
// birth date and no birth year.
type HouseholdDuplicateMember struct {
	Id            string `json:"id"`
	Name          string `json:"name"`
	Household     string `json:"household"`
	HouseholdName string `json:"householdName"`
}

// HouseholdDuplicateGroup is a set of people who may be the same person.
type HouseholdDuplicateGroup struct {
	// Reason is "same_name_birth_year" or "same_name_household".
	Reason  string                     `json:"reason"`
	Members []HouseholdDuplicateMember `json:"members"`
}

// NormalizeHouseholdName lowercases a name and keeps letters and digits, with
// single spaces between words ("  Mary-Ann  O'Neil " -> "maryann oneil").
func NormalizeHouseholdName(s string) string {
	words := strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return unicode.ToLower(r)
		case unicode.IsSpace(r):
			return ' '
		}

		return -1
	}, s))

	return strings.Join(words, " ")
}

// HouseholdDuplicates lists likely duplicate people among the active people of
// the active households of the org: the same normalized name and the same
// birth year, or the same name inside one household. The birth year is read
// (decrypted) here and never leaves this function. While the birth date field
// is still plain (no key), the year is not used. The list is capped; the
// second result tells whether it was cut.
func HouseholdDuplicates(app App, orgId string) ([]HouseholdDuplicateGroup, bool, error) {
	mcol, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers)
	if err != nil {
		return nil, false, err
	}

	sealed := false
	if f := mcol.Fields.GetByName("birthDate"); f != nil {
		sealed = f.Type() == FieldTypeEncryptedText
	}

	rows, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
		"household.org={:o} && household.status={:hs} && status={:s}", "created", 0, 0,
		dbx.Params{"o": orgId, "hs": HouseholdStatusActive, "s": HouseholdMemberActive})
	if err != nil {
		return nil, false, err
	}

	hhNames := map[string]string{}

	hhs, err := OrgHouseholds(app, orgId, 0, 0)
	if err != nil {
		return nil, false, err
	}

	for _, h := range hhs {
		hhNames[h.Id] = h.GetString("name")
	}

	type person struct {
		m    HouseholdDuplicateMember
		norm string
		year int
	}

	people := make([]person, 0, len(rows))

	for _, r := range rows {
		p := person{
			m: HouseholdDuplicateMember{
				Id: r.Id, Name: r.GetString("name"),
				Household: r.GetString("household"), HouseholdName: hhNames[r.GetString("household")],
			},
			norm: NormalizeHouseholdName(r.GetString("name")),
		}

		if sealed {
			if t, err := ParseBirthDate(r.GetString("birthDate")); err == nil {
				p.year = t.Year()
			}
		}

		if p.norm != "" {
			people = append(people, p)
		}
	}

	type group struct {
		reason string
		sortBy string
		ids    []string
		items  []HouseholdDuplicateMember
	}

	byYear := map[string]*group{}
	byHousehold := map[string]*group{}

	add := func(into map[string]*group, key, reason, sortBy string, p person) {
		g := into[key]
		if g == nil {
			g = &group{reason: reason, sortBy: sortBy}
			into[key] = g
		}

		g.ids = append(g.ids, p.m.Id)
		g.items = append(g.items, p.m)
	}

	for _, p := range people {
		if p.year != 0 {
			add(byYear, p.norm+"|"+strconv.Itoa(p.year), "same_name_birth_year", p.norm, p)
		}

		add(byHousehold, p.norm+"|"+p.m.Household, "same_name_household", p.norm, p)
	}

	var groups []*group

	seen := map[string]bool{}

	for _, src := range []map[string]*group{byYear, byHousehold} {
		for _, g := range src {
			if len(g.ids) < 2 {
				continue
			}

			slices.Sort(g.ids)

			key := strings.Join(g.ids, ",")
			if seen[key] {
				continue
			}

			seen[key] = true

			groups = append(groups, g)
		}
	}

	slices.SortFunc(groups, func(a, b *group) int {
		if c := strings.Compare(a.sortBy, b.sortBy); c != 0 {
			return c
		}

		return strings.Compare(a.ids[0], b.ids[0])
	})

	truncated := false
	if len(groups) > HouseholdDuplicatesLimit {
		groups, truncated = groups[:HouseholdDuplicatesLimit], true
	}

	out := make([]HouseholdDuplicateGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, HouseholdDuplicateGroup{Reason: g.reason, Members: g.items})
	}

	return out, truncated, nil
}

// ---- photos -------------------------------------------------------------------

// canEditHouseholdMemberPhoto follows UpdateHouseholdMember: staff, an active
// guardian of the household (any person), and the person themselves.
func canEditHouseholdMemberPhoto(actor *HouseholdActor, m *Record) bool {
	return actor.Staff || actor.Guardian() || (actor.Member != nil && actor.Member.Id == m.Id)
}

// SetHouseholdMemberPhoto stores the photo of a person. The field's own limits
// (image/jpeg, image/png, image/webp; 5 MB) are enforced by validation.
func SetHouseholdMemberPhoto(app App, ctx context.Context, actor *HouseholdActor, org, h, m *Record, file *filesystem.File) (*Record, error) {
	return setHouseholdMemberPhoto(app, ctx, actor, org, h, m, file)
}

// RemoveHouseholdMemberPhoto clears the photo of a person.
func RemoveHouseholdMemberPhoto(app App, ctx context.Context, actor *HouseholdActor, org, h, m *Record) (*Record, error) {
	return setHouseholdMemberPhoto(app, ctx, actor, org, h, m, nil)
}

func setHouseholdMemberPhoto(app App, ctx context.Context, actor *HouseholdActor, org, h, m *Record, file *filesystem.File) (*Record, error) {
	if m.GetString("household") != h.Id {
		return nil, householdErr(404, "Household member not found.")
	}

	if !canEditHouseholdMemberPhoto(actor, m) {
		return nil, householdErr(403, "You cannot change the photo of this person.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, err
	}

	if file != nil {
		m.Set("photo", file)
	} else {
		m.Set("photo", "")
	}

	if err := saveOrgRecord(app, ctx, m); err != nil {
		return nil, err
	}

	return m, nil
}

// ---- repair -------------------------------------------------------------------

// HouseholdCopyRepair is what a repair run found in one dependent collection.
type HouseholdCopyRepair struct {
	Collection string `json:"collection"`
	Drifted    int    `json:"drifted"`
}

// RepairHouseholdCopies makes the household copy of every dependent record
// equal to the household of its member (the member row is the truth). With
// dryRun it only counts. Records whose member no longer exists are left alone.
func RepairHouseholdCopies(app App, dryRun bool) ([]HouseholdCopyRepair, error) {
	deps, err := HouseholdDependents(app)
	if err != nil {
		return nil, err
	}

	out := make([]HouseholdCopyRepair, 0, len(deps))

	err = app.RunInTransaction(func(txApp App) error {
		for _, d := range deps {
			where := "EXISTS (SELECT 1 FROM {{" + CollectionNameHouseholdMembers + "}} hm WHERE hm.[[id]] = {{" + d.Collection + "}}.[[" + d.Member + "]]" +
				" AND hm.[[household]] <> COALESCE({{" + d.Collection + "}}.[[" + d.Household + "]], ''))"

			var n int
			if err := txApp.NonconcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + d.Collection + "}} WHERE " + where).Row(&n); err != nil {
				return err
			}

			if n > 0 && !dryRun {
				if _, err := txApp.NonconcurrentDB().NewQuery("UPDATE {{" + d.Collection + "}} SET [[" + d.Household + "]] = " +
					"(SELECT hm.[[household]] FROM {{" + CollectionNameHouseholdMembers + "}} hm WHERE hm.[[id]] = {{" + d.Collection + "}}.[[" + d.Member + "]]) WHERE " + where).Execute(); err != nil {
					return err
				}
			}

			out = append(out, HouseholdCopyRepair{Collection: d.Collection, Drifted: n})
		}

		return nil
	})

	return out, err
}
