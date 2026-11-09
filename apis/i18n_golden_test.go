package apis_test

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
)

// The golden files hold the English pages and emails exactly as master
// rendered them before localization (Claviger #97): the default language must
// not change. The only allowed difference is the inline JSON data block that
// carries the strings the page scripts need (stripped before comparing).
// Regenerate with -update-i18n-golden only for a deliberate change.
var updateI18nGolden = flag.Bool("update-i18n-golden", false, "rewrite the i18n golden files")

var i18nDataBlockRe = regexp.MustCompile(`(?s)<script type="application/json" id="i18n">.*?</script>\n?`)

func checkI18nGolden(t *testing.T, name, got string) {
	t.Helper()

	got = i18nDataBlockRe.ReplaceAllString(got, "")
	path := filepath.Join("testdata", "i18n_golden", name)

	if *updateI18nGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != got {
		t.Fatalf("%s differs from the golden file:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestEnglishHostedPagesAreIdenticalToTheGolden(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	for file, url := range map[string]string{
		"orgs_invite_existing.html": "/orgs/invite/" + invTokenFrank,
		"orgs_invite_signup.html":   "/orgs/invite/" + invTokenNewbie,
		"orgs_invite_invalid.html":  "/orgs/invite/nope",
		"orgs_admin.html":           "/orgs/parish-1",
		"orgs_notfound.html":        "/orgs/@",
	} {
		body, _ := servePage(t, app, url)
		checkI18nGolden(t, file, body)
	}
}

func TestEnglishPasskeysPageIsIdenticalToTheGolden(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	body, _ := servePage(t, app, "/passkeys")
	checkI18nGolden(t, "passkeys.html", body)
}

func TestEnglishInvitationEmailIsIdenticalToTheGolden(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	org, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	orgMust(t, err)

	inv, err := app.FindRecordById(core.CollectionNameInvitations, invFrank)
	orgMust(t, err)
	inv.Set("expires", time.Date(2031, 3, 9, 12, 0, 0, 0, time.UTC))

	orgMust(t, mails.SendOrgInvitation(app, org, inv, "Pastor Jo", "tok-golden"))

	m := app.TestMailer.LastMessage()
	checkI18nGolden(t, "invitation_email.txt", "To: "+m.To[0].Address+"\nSubject: "+m.Subject+"\n\n"+m.HTML)
}
