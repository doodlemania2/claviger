package mails_test

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tests"
)

// The golden files hold the English emails exactly as master rendered them
// before localization (Claviger #97): the default language must not change by
// a byte. Regenerate with -update-i18n-golden only for a deliberate change.
var updateGolden = flag.Bool("update-i18n-golden", false, "rewrite the i18n golden files")

var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func normalizeMail(s string) string { return jwtRe.ReplaceAllString(s, "JWT") }

func checkGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", "i18n_golden", name)
	if *updateGolden {
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

func TestEnglishEmailsAreByteIdenticalToTheGolden(t *testing.T) {
	t.Parallel()

	send := map[string]func(app core.App, user *core.Record) error{
		"verification":   func(app core.App, u *core.Record) error { return mails.SendRecordVerification(app, u) },
		"password_reset": func(app core.App, u *core.Record) error { return mails.SendRecordPasswordReset(app, u) },
		"email_change": func(app core.App, u *core.Record) error {
			return mails.SendRecordChangeEmail(app, u, "new@example.com")
		},
		"otp":           func(app core.App, u *core.Record) error { return mails.SendRecordOTP(app, u, "otpid", "123456") },
		"auth_alert":    func(app core.App, u *core.Record) error { return mails.SendRecordAuthAlert(app, u, "Chrome on Linux") },
		"passkey_reset": func(app core.App, u *core.Record) error { return mails.SendRecordPasskeyReset(app, u) },
	}

	for name, fn := range send {
		t.Run(name, func(t *testing.T) {
			app, _ := tests.NewTestApp()
			defer app.Cleanup()

			user := defaultTemplateUser(t, app)
			if err := fn(app, user); err != nil {
				t.Fatal(err)
			}

			m := app.TestMailer.LastMessage()
			checkGolden(t, name+".txt", "To: "+m.To[0].Address+"\nSubject: "+m.Subject+"\n\n"+normalizeMail(m.HTML))
		})
	}

}

// defaultTemplateUser is a member of a brand new auth collection: the fixture
// "users" collection carries customized templates, a new one the built-in defaults.
func defaultTemplateUser(t testing.TB, app core.App) *core.Record {
	t.Helper()

	col := core.NewAuthCollection("members")
	col.Fields.Add(&core.TextField{Name: "name"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	user := core.NewRecord(col)
	user.SetEmail("member@example.com")
	user.SetPassword("1234567890")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}

	return user
}
