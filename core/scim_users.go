package core

import (
	"context"
	"errors"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #79c): SCIM user provisioning.
//
// A SCIM User is an auth record of the org user collection plus a membership
// in the token's org (source "scim"). The functions here are the whole policy;
// apis/scim_users.go only translates HTTP and JSON.
//
// Rules, in short:
//
//   - userName (or, failing that, the work email) is the account email.
//   - An existing account is linked only when its email is verified; an
//     unverified one is a 409 uniqueness conflict. Linking never changes the
//     account's password or profile, except a name that is still empty.
//   - A new account is created verified (the identity provider asserted the
//     email) with a random password nobody knows: the user signs in with a
//     passkey, OIDC, an email code or a reset.
//   - The email and name of an account are changed only when the account is
//     owned by this org's SCIM (created by it and in no other org), so one
//     provider cannot rewrite an account that other orgs also rely on.
//   - active=false suspends the membership; the org setting
//     scimDeactivationBans also bans the account, unless the account is a
//     member of another active org or the ban was set by someone else.
//   - DELETE removes the membership, never the account.
//   - SCIM never grants ownership. It may suspend or remove an owner (the
//     last-owner guard still applies).

// SCIMError is a failure that maps to an RFC 7644 error response.
type SCIMError struct {
	Status int
	// Type is the scimType ("" for none).
	Type   string
	Detail string
}

func (e *SCIMError) Error() string { return e.Detail }

func scimErr(status int, typ, detail string) error {
	return &SCIMError{Status: status, Type: typ, Detail: detail}
}

// AsSCIMError converts any error of the provisioning functions.
func AsSCIMError(err error) *SCIMError {
	var se *SCIMError
	if errors.As(err, &se) {
		return se
	}

	if oe, ok := AsOrgError(err); ok {
		return &SCIMError{Status: oe.Status, Detail: oe.Message}
	}

	return &SCIMError{Status: 500, Detail: "Internal error."}
}

// SCIMUserAttrs are the attributes of a User resource that Claviger uses.
// nil means "not provided".
type SCIMUserAttrs struct {
	UserName    *string
	WorkEmail   *string
	ExternalId  *string
	DisplayName *string
	GivenName   *string
	FamilyName  *string
	Formatted   *string
	Active      *bool
}

func (a *SCIMUserAttrs) email() string {
	if a.UserName != nil && strings.TrimSpace(*a.UserName) != "" {
		return *a.UserName
	}

	if a.WorkEmail != nil {
		return *a.WorkEmail
	}

	return ""
}

func (a *SCIMUserAttrs) hasName() bool {
	return a.DisplayName != nil || a.GivenName != nil || a.FamilyName != nil || a.Formatted != nil
}

// name combines the name attributes; current is used for the parts that are
// not provided.
func (a *SCIMUserAttrs) name() string {
	if a.DisplayName != nil && strings.TrimSpace(*a.DisplayName) != "" {
		return strings.TrimSpace(*a.DisplayName)
	}

	if a.Formatted != nil && strings.TrimSpace(*a.Formatted) != "" {
		return strings.TrimSpace(*a.Formatted)
	}

	given, family := "", ""
	if a.GivenName != nil {
		given = *a.GivenName
	}

	if a.FamilyName != nil {
		family = *a.FamilyName
	}

	return strings.TrimSpace(given + " " + family)
}

func scimNameField(col *Collection) bool {
	_, ok := col.Fields.GetByName("name").(*TextField)

	return ok
}

func scimSystemActor() *OrgActor { return &OrgActor{System: true} }

// SCIMOwnsAccount reports whether this org's SCIM created the account and no
// other org uses it.
func SCIMOwnsAccount(app App, member, user *Record) bool {
	if member.GetString("source") != OrgSourceSCIM {
		return false
	}

	n, err := app.CountRecords(CollectionNameOrgMembers, dbx.HashExp{"user": user.Id})

	return err == nil && n == 1
}

// FindSCIMMember returns the membership of the user in the org together with
// the user, or a 404 SCIMError.
func FindSCIMMember(app App, org *Record, userId string) (member, user *Record, err error) {
	member, mErr := FindOrgMember(app, org.Id, userId)
	if mErr != nil {
		return nil, nil, scimErr(404, "", "Resource "+userId+" not found.")
	}

	col, cErr := OrgUserCollection(app)
	if cErr != nil {
		return nil, nil, scimErr(500, "", "SCIM is not initialized.")
	}

	user, uErr := app.FindRecordById(col, userId)
	if uErr != nil {
		return nil, nil, scimErr(404, "", "Resource "+userId+" not found.")
	}

	return member, user, nil
}

func scimExternalIdTaken(app App, org *Record, externalId, exceptMemberId string) bool {
	if externalId == "" {
		return false
	}

	m, err := app.FindFirstRecordByFilter(CollectionNameOrgMembers, "org={:o} && externalId={:e}",
		dbx.Params{"o": org.Id, "e": externalId})

	return err == nil && m.Id != exceptMemberId
}

// SCIMCreateUser provisions a user (create or link) in the org.
func SCIMCreateUser(app App, ctx context.Context, org *Record, a SCIMUserAttrs) (member, user *Record, err error) {
	raw := a.email()
	if strings.TrimSpace(raw) == "" {
		return nil, nil, scimErr(400, "invalidValue", "userName is required.")
	}

	email, nerr := NormalizeInvitationEmail(raw)
	if nerr != nil {
		return nil, nil, scimErr(400, "invalidValue", "userName must be an email address.")
	}

	col, cerr := OrgUserCollection(app)
	if cerr != nil {
		return nil, nil, scimErr(500, "", "SCIM is not initialized.")
	}

	externalId := ""
	if a.ExternalId != nil {
		externalId = strings.TrimSpace(*a.ExternalId)
	}

	if scimExternalIdTaken(app, org, externalId, "") {
		return nil, nil, scimErr(409, "uniqueness", "A user with that externalId already exists in this organization.")
	}

	settings := GetSCIMSettings(org)
	active := a.Active == nil || *a.Active
	status := OrgStatusActive

	if !active {
		status = OrgStatusSuspended
	}

	existing, _ := app.FindAuthRecordByEmail(col, email)

	txErr := app.RunInTransaction(func(txApp App) error {
		if existing != nil {
			if _, mErr := FindOrgMember(txApp, org.Id, existing.Id); mErr == nil {
				return scimErr(409, "uniqueness", "The user already exists in this organization.")
			}

			if !existing.Verified() {
				return scimErr(409, "uniqueness",
					"An account with that email already exists but its email is not verified, so it cannot be linked. "+
						"The user must verify the email first.")
			}

			user = existing

			// only fill what is empty; never overwrite the user's own data
			if scimNameField(col) && a.hasName() && user.GetString("name") == "" {
				if n := a.name(); n != "" {
					user.Set("name", n)

					if err := txApp.SaveWithContext(ctx, user); err != nil {
						return scimErr(400, "invalidValue", err.Error())
					}
				}
			}
		} else {
			user = NewRecord(col)
			user.SetEmail(email)
			user.SetRandomPassword()
			user.SetVerified(true)

			if scimNameField(col) {
				if n := a.name(); n != "" {
					user.Set("name", n)
				}
			}

			if err := txApp.SaveWithContext(ctx, user); err != nil {
				if existsAfterConflict(txApp, col, email) {
					return scimErr(409, "uniqueness", "An account with that email already exists.")
				}

				return scimErr(400, "invalidValue", err.Error())
			}
		}

		var mErr error

		member, mErr = AddOrgMember(txApp, ctx, scimSystemActor(), org, OrgMemberInput{
			User: user.Id, Role: settings.DefaultRole, Status: status, Source: OrgSourceSCIM, ExternalId: externalId,
		})

		return mErr
	})
	if txErr != nil {
		return nil, nil, txErr
	}

	if !active {
		if err := scimApplyDeactivation(app, ctx, org, user, true); err != nil {
			return nil, nil, err
		}
	}

	return member, user, nil
}

func existsAfterConflict(app App, col *Collection, email string) bool {
	_, err := app.FindAuthRecordByEmail(col, email)

	return err == nil
}

// SCIMUpdateUser applies the provided attributes to a member.
func SCIMUpdateUser(app App, ctx context.Context, org, member, user *Record, a SCIMUserAttrs) (*Record, *Record, error) {
	col := user.Collection()
	owns := SCIMOwnsAccount(app, member, user)

	var newEmail string

	if raw := a.email(); strings.TrimSpace(raw) != "" {
		norm, nerr := NormalizeInvitationEmail(raw)
		if nerr != nil {
			return nil, nil, scimErr(400, "invalidValue", "userName must be an email address.")
		}

		if !strings.EqualFold(norm, user.Email()) {
			if !owns {
				return nil, nil, scimErr(400, "mutability",
					"The email of this account cannot be changed by SCIM: the account is not owned by this organization's provisioning.")
			}

			if other, err := app.FindAuthRecordByEmail(col, norm); err == nil && other.Id != user.Id {
				return nil, nil, scimErr(409, "uniqueness", "An account with that email already exists.")
			}

			newEmail = norm
		}
	}

	patch := OrgMemberPatch{}

	if a.ExternalId != nil {
		ext := strings.TrimSpace(*a.ExternalId)
		if scimExternalIdTaken(app, org, ext, member.Id) {
			return nil, nil, scimErr(409, "uniqueness", "A user with that externalId already exists in this organization.")
		}

		if ext != member.GetString("externalId") {
			patch.ExternalId = &ext
		}
	}

	wantActive := member.GetString("status") == OrgStatusActive
	activeChange := false

	if a.Active != nil {
		wantActive = *a.Active
		activeChange = true

		st := OrgStatusSuspended
		if wantActive {
			st = OrgStatusActive
		}

		if st != member.GetString("status") {
			patch.Status = &st
		}
	}

	txErr := app.RunInTransaction(func(txApp App) error {
		userChanged := false

		if newEmail != "" {
			user.SetEmail(newEmail)

			userChanged = true
		}

		if owns && a.hasName() && scimNameField(col) {
			if n := a.name(); n != user.GetString("name") {
				user.Set("name", n)

				userChanged = true
			}
		}

		if userChanged {
			if err := txApp.SaveWithContext(ctx, user); err != nil {
				return scimErr(400, "invalidValue", err.Error())
			}
		}

		if patch.ExternalId != nil || patch.Status != nil {
			updated, err := UpdateOrgMember(txApp, ctx, scimSystemActor(), org, member, patch)
			if err != nil {
				return err
			}

			member = updated
		}

		return nil
	})
	if txErr != nil {
		return nil, nil, txErr
	}

	if activeChange {
		if err := scimApplyDeactivation(app, ctx, org, user, !wantActive); err != nil {
			return nil, nil, err
		}
	}

	return member, user, nil
}

// SCIMDeleteUser removes the membership (never the account) and the user from
// the org's SCIM groups.
func SCIMDeleteUser(app App, ctx context.Context, org, member, user *Record) error {
	return app.RunInTransaction(func(txApp App) error {
		if err := RemoveOrgMember(txApp, ctx, scimSystemActor(), org, member); err != nil {
			return err
		}

		return scimStripUserFromGroups(txApp, ctx, org, user.Id)
	})
}

// ---- deactivation ----------------------------------------------------------

func scimBanOwner(org *Record) string { return "scim:" + org.Id }

// scimApplyDeactivation bans (deactivate=true, setting on) or lifts this
// org's own ban (deactivate=false). The membership status is changed by the
// caller.
func scimApplyDeactivation(app App, ctx context.Context, org, user *Record, deactivate bool) error {
	if !deactivate {
		return scimLiftBan(app, ctx, org, user)
	}

	if !GetSCIMSettings(org).DeactivationBans {
		return nil
	}

	// never ban a superuser, and never lock an account out of other orgs
	if user.IsSuperuser() {
		return nil
	}

	others, err := ActiveOrgMemberships(app, user.Id)
	if err != nil {
		return scimErr(500, "", "Failed to check the memberships.")
	}

	for _, m := range others {
		if m.GetString("org") != org.Id {
			return nil
		}
	}

	if FindActiveBan(app, user.Collection().Id, user.Id) != nil {
		return nil
	}

	bans, err := app.FindCachedCollectionByNameOrId(CollectionNameBans)
	if err != nil {
		return scimErr(500, "", "Bans are not available.")
	}

	ban, err := app.FindFirstRecordByFilter(CollectionNameBans, "collectionRef={:c} && recordRef={:r}",
		dbx.Params{"c": user.Collection().Id, "r": user.Id})
	if err != nil {
		ban = NewRecord(bans)
		ban.Set("collectionRef", user.Collection().Id)
		ban.Set("recordRef", user.Id)
	}

	ban.Set("reason", "Deactivated by SCIM ("+org.GetString("slug")+")")
	ban.Set("until", "")
	ban.Set("createdBy", scimBanOwner(org))

	if err := app.SaveWithContext(ctx, ban); err != nil {
		return scimErr(500, "", "Failed to ban the account.")
	}

	return nil
}

func scimLiftBan(app App, ctx context.Context, org, user *Record) error {
	ban, err := app.FindFirstRecordByFilter(CollectionNameBans, "collectionRef={:c} && recordRef={:r} && createdBy={:by}",
		dbx.Params{"c": user.Collection().Id, "r": user.Id, "by": scimBanOwner(org)})
	if err != nil {
		return nil
	}

	if err := app.DeleteWithContext(ctx, ban); err != nil {
		return scimErr(500, "", "Failed to lift the ban.")
	}

	return nil
}
