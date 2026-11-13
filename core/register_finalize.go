package core

import (
	"context"
	"slices"
	"strings"

	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #87, 87-1): finalizing an entry.
//
// A register has one or two signers (the signers setting):
//
//   - One signer (default): a member tagged "registrar" finalizes alone.
//   - Two signers: a registrar requests the finalization (the entry becomes
//     "pending finalize", is not final and stays editable). A second, different
//     person who is also a registrar confirms. If the
//     entry changed after the request, the confirmation is refused and the
//     request has to be made again, so the second person signs what they saw.
//
// Every step writes an audit entry with meta.orgId. Only people with the
// registrar tag sign: owners and admins without the tag cannot, and a superuser
// can never be either signer (nor annotate); superusers keep their other
// abilities, such as setting registers up.

const errSuperuserSigns = "Superusers cannot sign or annotate registers. Ask a registrar of the org."

// RegisterFinalizeResult is the outcome of FinalizeRegisterEntry.
type RegisterFinalizeResult struct {
	Entry *Record
	// Pending is true when a second signer is still needed.
	Pending bool
	// Final is true when the entry is now final.
	Final bool
}

type registerStanding struct {
	actor     *OrgActor
	registrar bool // tagged "registrar": may start, confirm and annotate
	superuser bool
	staff     bool // may read through the register routes
}

func registerStandingOf(app App, org *Record, auth *Record) registerStanding {
	st := registerStanding{actor: NewOrgActor(app, org, auth)}

	if st.actor.Superuser {
		st.superuser, st.staff = true, true

		return st
	}

	if !st.actor.IsMember() {
		return st
	}

	if m := ActiveOrgMember(app, org, auth.Id); m != nil && slices.Contains(OrgTagNames(app, m), RegistrarTag) {
		st.registrar = true
	}

	st.staff = st.registrar || st.actor.Role == OrgRoleOwner || st.actor.Role == OrgRoleAdmin

	return st
}

// registerEntry loads the configuration, the entry and the org, and checks that
// the caller is allowed to know about them.
func registerEntry(app App, collection, id string, auth *Record) (*PermanentConfig, *Record, *Record, registerStanding, error) {
	var none registerStanding

	cfg := PermanentConfigFor(app, collection)
	if cfg == nil || auth == nil {
		return nil, nil, nil, none, orgErr(404, "Register not found.")
	}

	entry, err := app.FindRecordById(collection, id)
	if err != nil {
		return nil, nil, nil, none, orgErr(404, "Entry not found.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, entry.GetString("org"))
	if err != nil || org.GetBool("archived") {
		return nil, nil, nil, none, orgErr(404, "Entry not found.")
	}

	st := registerStandingOf(app, org, auth)
	if !st.actor.IsMember() {
		return nil, nil, nil, none, orgErr(404, "Entry not found.")
	}

	return cfg, entry, org, st, nil
}

// RegisterStaff reports whether the caller may read the entry through the
// register routes (owners, admins, registrars, superusers of the entry's org).
func RegisterStaff(app App, collection, id string, auth *Record) (*PermanentConfig, *Record, *Record, error) {
	cfg, entry, org, st, err := registerEntry(app, collection, id, auth)
	if err != nil {
		return nil, nil, nil, err
	}

	if !st.staff {
		return nil, nil, nil, orgErr(403, "Only registrars, owners and admins can do this.")
	}

	return cfg, entry, org, nil
}

// FinalizeRegisterEntry takes the next step of finalizing an entry for the
// caller: it finalizes (one signer), requests (two signers, first step) or
// confirms (two signers, second step).
func FinalizeRegisterEntry(app App, ctx context.Context, auth *Record, collection, id string) (*RegisterFinalizeResult, error) {
	var res *RegisterFinalizeResult

	err := app.RunInTransaction(func(txApp App) error {
		cfg, entry, _, st, err := registerEntry(txApp, collection, id, auth)
		if err != nil {
			return err
		}

		if cfg.IsFinalized(entry) {
			return orgErr(409, "This entry is already final.")
		}

		if st.superuser {
			return orgErr(403, errSuperuserSigns)
		}

		pendingBy := entry.GetString("pendingBy")

		switch {
		case cfg.Signers == 2 && pendingBy != "":
			if !st.registrar {
				return orgErr(403, "Only registrars can confirm a finalization.")
			}

			if pendingBy == auth.Id {
				return orgErr(409, "A different person has to confirm this entry. Your request is waiting.")
			}

			if entry.GetString("pendingHash") != registerContentHash(entry, cfg) {
				return orgErr(409, "The entry changed after it was put forward. Ask for the finalization again.")
			}

			res, err = finishRegisterEntry(txApp, ctx, cfg, entry, pendingBy, auth.Id)

			return err
		default:
			if !st.registrar {
				return orgErr(403, "Only registrars can finalize entries.")
			}
		}

		if cfg.Signers == 2 {
			entry.Set("pendingBy", auth.Id)
			entry.Set("pendingAt", types.NowDateTime())
			entry.Set("pendingHash", registerContentHash(entry, cfg))

			if err := txApp.SaveWithContext(registerWriteContext(ctx), entry); err != nil {
				return wrapOrgSaveErr(err)
			}

			res = &RegisterFinalizeResult{Entry: entry, Pending: true}

			return writeRegisterAudit(txApp, ctx, AuditActionRegisterFinalizeRequest, cfg, entry, map[string]any{"signers": 2})
		}

		res, err = finishRegisterEntry(txApp, ctx, cfg, entry, auth.Id, "")

		return err
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func finishRegisterEntry(app App, ctx context.Context, cfg *PermanentConfig, entry *Record, first, second string) (*RegisterFinalizeResult, error) {
	entry.Set(cfg.FinalizeField, types.NowDateTime())
	entry.Set("finalizedBy", first)
	entry.Set("secondSigner", second)
	entry.Set("pendingBy", "")
	entry.Set("pendingAt", "")
	entry.Set("pendingHash", "")

	var seq int

	if cfg.Chain {
		last, prev, err := registerChainTail(app, cfg)
		if err != nil {
			return nil, err
		}

		seq = last + 1
		entry.Set("seq", seq)
		entry.Set("hash", registerChainHash(prev, entry, cfg))
	}

	if err := app.SaveWithContext(registerWriteContext(ctx), entry); err != nil {
		return nil, wrapOrgSaveErr(err)
	}

	extra := map[string]any{"signers": cfg.Signers, "finalizedBy": first}
	if second != "" {
		extra["secondSigner"] = second
	}

	if cfg.Chain {
		extra["seq"] = seq
		extra["hash"] = entry.GetString("hash")
	}

	if err := writeRegisterAudit(app, ctx, AuditActionRegisterFinalize, cfg, entry, extra); err != nil {
		return nil, err
	}

	return &RegisterFinalizeResult{Entry: entry, Final: true}, nil
}

// WithdrawRegisterFinalize cancels a pending finalization. The person who put
// it forward, an owner or admin, or a superuser may do it.
func WithdrawRegisterFinalize(app App, ctx context.Context, auth *Record, collection, id string) (*Record, error) {
	var out *Record

	err := app.RunInTransaction(func(txApp App) error {
		cfg, entry, _, st, err := registerEntry(txApp, collection, id, auth)
		if err != nil {
			return err
		}

		if cfg.IsFinalized(entry) || entry.GetString("pendingBy") == "" {
			return orgErr(409, "No finalization is waiting for this entry.")
		}

		manager := st.actor.Superuser || st.actor.Role == OrgRoleOwner || st.actor.Role == OrgRoleAdmin
		if entry.GetString("pendingBy") != auth.Id && !manager {
			return orgErr(403, "Only the person who asked, an owner or an admin can withdraw the request.")
		}

		by := entry.GetString("pendingBy")

		entry.Set("pendingBy", "")
		entry.Set("pendingAt", "")
		entry.Set("pendingHash", "")

		if err := txApp.SaveWithContext(registerWriteContext(ctx), entry); err != nil {
			return wrapOrgSaveErr(err)
		}

		out = entry

		return writeRegisterAudit(txApp, ctx, AuditActionRegisterFinalizeWithdraw, cfg, entry, map[string]any{"requestedBy": by})
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// RegisterState names the state of an entry: draft, pending or final.
func RegisterState(cfg *PermanentConfig, entry *Record) string {
	switch {
	case cfg.IsFinalized(entry):
		return "final"
	case strings.TrimSpace(entry.GetString("pendingBy")) != "":
		return "pending"
	}

	return "draft"
}
