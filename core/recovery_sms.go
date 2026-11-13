package core

import (
	"context"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/sms"
)

// Fork-local (Claviger #83, 83-5): recovery by text message.
//
// The phone number is never typed by staff and never stored anew. It is the
// home phone of the household the person belongs to (an encrypted field), read
// here at send time. It is not returned by any route and not written to the
// audit log.

// AuditActionRecoverySMS is written once per text message sent for a recovery.
// meta.kind is "link" or "notice"; the number and the link are never in it.
const AuditActionRecoverySMS = "recovery_sms_sent"

// SMSSenderStoreKey lets a test put a fake [sms.Sender] in app.Store(). It is
// consulted by [RecoverySMSSender] only; a production app never sets it.
const SMSSenderStoreKey = "__clvSMSSender__"

// RecoverySMSSender returns the configured SMS sender, or
// [ErrSMSNotConfigured].
func RecoverySMSSender(app App) (sms.Sender, error) {
	if s, ok := app.Store().Get(SMSSenderStoreKey).(sms.Sender); ok && s != nil {
		return s, nil
	}

	return NewSMSSender(app)
}

// RecoverySMSConfigured reports whether a text message could be sent at all.
func RecoverySMSConfigured(app App) bool {
	_, err := RecoverySMSSender(app)

	return err == nil
}

// NormalizePhone strips the usual formatting (spaces, dashes, dots, brackets)
// and returns the number when what is left is E.164. A number without a
// country code is refused, because guessing one would text a stranger.
func NormalizePhone(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '.', '(', ')', ' ':
			return -1
		}

		return r
	}, strings.TrimSpace(s))

	if sms.ValidateE164(s) != nil {
		return ""
	}

	return s
}

// RecoveryPhone is the number to text for the person: the home phone of an
// active household of org in which they are an active member. Empty means
// there is none (or it is not a usable number).
func RecoveryPhone(app App, org, user *Record) string {
	if org == nil || user == nil || !HouseholdsAvailable(app) {
		return ""
	}

	rows, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
		"user={:u} && status={:s} && household.status={:hs} && household.org={:o}", "created", 0, 0,
		dbx.Params{"u": user.Id, "s": HouseholdMemberActive, "hs": HouseholdStatusActive, "o": org.Id})
	if err != nil {
		return ""
	}

	for _, m := range rows {
		h, err := app.FindRecordById(CollectionNameHouseholds, m.GetString("household"))
		if err != nil {
			continue
		}

		if p := NormalizePhoneRegion(h.GetString("homePhone"), app.Settings().SMS.DefaultRegion); p != "" {
			return p
		}
	}

	return ""
}

// requireRecoverySMS refuses the sms channel, before any code exists, when the
// message could not be sent. The wording does not reveal the number.
func requireRecoverySMS(app App, org, target *Record) error {
	if !RecoverySMSConfigured(app) {
		return orgErr(400, "Text messages are not set up. Choose another way to give the code.")
	}

	if RecoveryPhone(app, org, target) == "" {
		return orgErr(400, "This person has no phone number on their household. Choose another way to give the code.")
	}

	return nil
}

// SendRecoverySMS texts body to the person's household phone and writes one
// audit entry. kind is "link" or "notice". It reports whether a message was
// sent; a failure is returned without the number in it.
func SendRecoverySMS(app App, ctx context.Context, org, target, rec *Record, kind, body string) (bool, error) {
	to := RecoveryPhone(app, org, target)
	if to == "" {
		return false, nil
	}

	sender, err := RecoverySMSSender(app)
	if err != nil {
		return false, err
	}

	if err := sender.Send(ctx, &sms.Message{To: to, Body: body}); err != nil {
		return false, err
	}

	return true, writeRecoveryAudit(app, ctx, AuditActionRecoverySMS, rec, map[string]any{"kind": kind})
}
