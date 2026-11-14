package core

import (
	"fmt"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #87, 87-5): read helpers for the register screens (the
// admin UI and the hosted registrar page). They only read; every write still
// goes through the finalize and annotation functions.

// RegisterAccess is what a person may do with the registers of one org.
type RegisterAccess struct {
	Org       *Record
	Staff     bool // owner, admin, registrar (or a superuser): may read
	Registrar bool // may finalize and annotate
	Superuser bool
}

// RegisterAccessFor resolves the standing of auth in an org (an id). A
// superuser may read every org; anyone else must be staff of it, and a member
// who is not staff, or a stranger, is told the org does not exist.
func RegisterAccessFor(app App, orgId string, auth *Record) (*RegisterAccess, error) {
	if auth == nil {
		return nil, orgErr(401, "Sign in first.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, orgId)
	if err != nil || org.GetBool("archived") {
		return nil, orgErr(404, "Organization not found.")
	}

	st := registerStandingOf(app, org, auth)
	if !st.actor.IsMember() {
		return nil, orgErr(404, "Organization not found.")
	}

	if !st.staff {
		return nil, orgErr(403, "Only registrars, owners and admins can do this.")
	}

	return &RegisterAccess{Org: org, Staff: true, Registrar: st.registrar, Superuser: st.superuser}, nil
}

// RegisterListFilter selects the entries of a register for a list.
type RegisterListFilter struct {
	Org     string // required for everyone but a superuser
	State   string // "", "draft", "pending" or "final"
	Page    int
	PerPage int
}

// RegisterEntryList is one page of entries.
type RegisterEntryList struct {
	Page       int
	PerPage    int
	TotalItems int
	Items      []*Record
}

// ListRegisterEntries lists the entries of a register, newest first. The
// caller has checked the standing (RegisterAccessFor).
func ListRegisterEntries(app App, cfg *PermanentConfig, f RegisterListFilter) (*RegisterEntryList, error) {
	if !registerNameRegex.MatchString(cfg.FinalizeField) {
		return nil, orgErr(500, "The register is misconfigured.")
	}

	if f.Page < 1 {
		f.Page = 1
	}

	if f.PerPage < 1 || f.PerPage > 200 {
		f.PerPage = 50
	}

	col, err := app.FindCachedCollectionByNameOrId(cfg.Collection)
	if err != nil {
		return nil, err
	}

	fin := fmt.Sprintf("[[%s]]", cfg.FinalizeField)

	var (
		exprs   []dbx.Expression
		filters []string
		params  = dbx.Params{}
	)

	if f.Org != "" {
		exprs = append(exprs, dbx.NewExp("[[org]] = {:org}", dbx.Params{"org": f.Org}))
		filters = append(filters, "org = {:org}")
		params["org"] = f.Org
	}

	switch f.State {
	case "":
	case "final":
		exprs = append(exprs, dbx.NewExp(fin+" != ''"))
		filters = append(filters, cfg.FinalizeField+" != ''")
	case "pending":
		exprs = append(exprs, dbx.NewExp(fin+" = '' AND [[pendingBy]] != ''"))
		filters = append(filters, cfg.FinalizeField+" = '' && pendingBy != ''")
	case "draft":
		exprs = append(exprs, dbx.NewExp(fin+" = '' AND [[pendingBy]] = ''"))
		filters = append(filters, cfg.FinalizeField+" = '' && pendingBy = ''")
	default:
		return nil, orgErr(400, "Unknown state. Use draft, pending or final.")
	}

	total, err := app.CountRecords(col, exprs...)
	if err != nil {
		return nil, err
	}

	items, err := app.FindRecordsByFilter(col, strings.Join(filters, " && "), "-@rowid", f.PerPage, (f.Page-1)*f.PerPage, params)
	if err != nil {
		return nil, err
	}

	return &RegisterEntryList{Page: f.Page, PerPage: f.PerPage, TotalItems: int(total), Items: items}, nil
}

// RegisterDisplayFields names the fields of a register that a screen may show
// as columns: not hidden, not the bookkeeping of finalizing, not an autodate.
func RegisterDisplayFields(app App, cfg *PermanentConfig) []map[string]string {
	col, err := app.FindCachedCollectionByNameOrId(cfg.Collection)
	if err != nil {
		return nil
	}

	var out []map[string]string

	for _, f := range col.Fields {
		name := f.GetName()

		if f.GetHidden() || name == "id" || name == "org" || name == cfg.FinalizeField || cfg.isManaged(name) || name == "restricted" {
			continue
		}

		if _, ok := f.(*AutodateField); ok {
			continue
		}

		out = append(out, map[string]string{"name": name, "type": f.Type()})
	}

	return out
}
