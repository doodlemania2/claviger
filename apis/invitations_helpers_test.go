package apis_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79b): fixtures of the invitation tests. They build on
// the people and organizations of orgs_helpers_test.go.

const (
	invTokenOK      = "tok-pending-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	invTokenExpired = "tok-expired-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	invTokenRevoked = "tok-revoked-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	invTokenUsed    = "tok-used-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	invTokenNewbie  = "tok-newbie-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	invTokenFrank   = "tok-frank-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	invTokenOther   = "tok-otherorg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	invOK      = "inv000000000001"
	invExpired = "inv000000000002"
	invRevoked = "inv000000000003"
	invUsed    = "inv000000000004"
	invNewbie  = "inv000000000005"
	invFrank   = "inv000000000006"
	invOther   = "inv000000000007"
)

var invTokenRe = regexp.MustCompile(`/orgs/invite/([A-Za-z0-9_-]+)`)

// seedInvitation writes an invitation with a known token.
func seedInvitation(t testing.TB, app core.App, id, org, email, role, token, status string, expires time.Time, sentAgo time.Duration) *core.Record {
	t.Helper()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameInvitations)
	orgMust(t, err)

	r := core.NewRecord(col)
	r.Id = id
	r.Set("org", org)
	r.Set("email", email)
	r.Set("role", role)
	r.Set("tokenHash", core.HashInvitationToken(token))
	r.Set("status", status)
	r.Set("expires", expires)
	r.Set("lastSentAt", time.Now().Add(-sentAgo))
	r.Set("sendCount", 1)

	if status == core.InvitationStatusAccepted {
		r.Set("acceptedBy", orgUserID("erin"))
		r.Set("acceptedAt", time.Now())
	}

	orgMust(t, app.Save(r))

	return r
}

// invFactory is orgsFactory plus a set of invitations.
func invFactory(t testing.TB) *tests.TestApp {
	app := orgsFactory(t)
	week := time.Now().Add(7 * 24 * time.Hour)

	seedInvitation(t, app, invOK, orgP1, "newhire@orgs.test", "member", invTokenOK, "pending", week, 10*time.Minute)
	seedInvitation(t, app, invExpired, orgP1, "late@orgs.test", "member", invTokenExpired, "pending", time.Now().Add(-time.Hour), 8*24*time.Hour)
	seedInvitation(t, app, invRevoked, orgP1, "gone@orgs.test", "member", invTokenRevoked, "revoked", week, 10*time.Minute)
	seedInvitation(t, app, invUsed, orgP1, "erin@orgs.test", "viewer", invTokenUsed, "accepted", week, 10*time.Minute)
	seedInvitation(t, app, invNewbie, orgP1, "newbie@orgs.test", "member", invTokenNewbie, "pending", week, 10*time.Minute)
	seedInvitation(t, app, invFrank, orgP1, "frank@orgs.test", "admin", invTokenFrank, "pending", week, 10*time.Minute)
	seedInvitation(t, app, invOther, orgP2, "other@orgs.test", "member", invTokenOther, "pending", week, 10*time.Minute)

	return app
}

// sentLinkToken extracts the token of the n-th sent invitation email.
func sentLinkToken(t testing.TB, app *tests.TestApp, n int) string {
	t.Helper()

	msgs := app.TestMailer.Messages()
	if len(msgs) <= n {
		t.Fatalf("expected at least %d emails, got %d", n+1, len(msgs))
	}

	m := invTokenRe.FindStringSubmatch(msgs[n].HTML)
	if m == nil {
		t.Fatalf("no invitation link in %q", msgs[n].HTML)
	}

	return m[1]
}

func invStatus(t testing.TB, app core.App, id string) string {
	t.Helper()

	r, err := app.FindRecordById(core.CollectionNameInvitations, id)
	orgMust(t, err)

	return r.GetString("status")
}

func invCase(c orgCase) orgCase {
	if c.factory == nil {
		c.factory = invFactory
	}

	return c
}

func runInvCases(t *testing.T, cases []orgCase) {
	t.Helper()

	for i := range cases {
		cases[i] = invCase(cases[i])
	}

	runOrgCases(t, cases)
}

func invContains(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}

	return true
}
