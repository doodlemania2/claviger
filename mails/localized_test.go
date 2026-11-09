package mails_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/i18n"
)

// Fork-local (Claviger #97): the built-in emails in the language of the recipient.

func TestEnglishBuiltinsEqualTheStoredDefaults(t *testing.T) {
	t.Parallel()

	b := i18n.Default()

	for _, kind := range []string{
		core.EmailKindVerification, core.EmailKindPasswordReset, core.EmailKindEmailChange,
		core.EmailKindOTP, core.EmailKindAuthAlert, core.EmailKindPasskeyReset,
	} {
		want, _ := core.DefaultEmailTemplate(kind)

		got, ok := mails.BuiltinTemplate(b, "en", kind)
		if !ok || got != want {
			t.Errorf("%s: the English catalog template differs from the built-in default\n got: %#v\nwant: %#v", kind, got, want)
		}
	}

	if got := mails.BuiltinInvitationTemplate(b, "en"); got != mails.DefaultOrgInvitationTemplate {
		t.Errorf("invitation: the English catalog template differs from the default\n got: %#v\nwant: %#v", got, mails.DefaultOrgInvitationTemplate)
	}
}

// membersApp has an auth collection with the built-in templates and a language field.
func membersApp(t *testing.T, langField bool) (*tests.TestApp, *core.Record) {
	t.Helper()

	app, _ := tests.NewTestApp()
	t.Cleanup(app.Cleanup)

	col := core.NewAuthCollection("members")
	if langField {
		col.Fields.Add(&core.TextField{Name: "lang"})
	}

	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	user := core.NewRecord(col)
	user.SetEmail("member@example.com")
	user.SetPassword("1234567890")

	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}

	return app, user
}

func TestBuiltinEmailsFollowTheRecordLanguage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		lang string
		rtl  bool
	}{
		{"es", false},
		{"pt-BR", false},
		{"ar", true},
		{"zh-Hans", false},
		{"pt-PT", false}, // matched to pt-BR
	} {
		t.Run(tc.lang, func(t *testing.T) {
			app, user := membersApp(t, true)
			user.Set("lang", tc.lang)

			if err := mails.SendRecordVerification(app, user); err != nil {
				t.Fatal(err)
			}

			m := app.TestMailer.LastMessage()

			want := core.I18n(app).T(mustResolve(t, app, tc.lang), "email.verification.subject", i18n.Args{"APP_NAME": "acme_test"})
			if m.Subject != want || m.Subject == "Verify your acme_test email" {
				t.Fatalf("subject %q, want %q", m.Subject, want)
			}

			if strings.Contains(m.HTML, "Click on the button below") {
				t.Fatal("the body is still English")
			}

			if !strings.Contains(m.HTML, "acme_test") || !strings.Contains(m.HTML, "/_/#/auth/confirm-verification/") {
				t.Fatalf("placeholders were not resolved: %s", m.HTML)
			}

			if strings.Contains(m.HTML, "{APP_NAME}") || strings.Contains(m.HTML, "{TOKEN}") {
				t.Fatalf("unresolved placeholder in %s", m.HTML)
			}

			if rtl := strings.Contains(m.HTML, `dir="rtl"`); rtl != tc.rtl {
				t.Fatalf("dir=rtl is %v, want %v", rtl, tc.rtl)
			}

			if !strings.Contains(m.HTML, `lang="`+mustResolve(t, app, tc.lang)+`"`) {
				t.Fatalf("the wrapper must carry the language: %s", m.HTML)
			}
		})
	}
}

func mustResolve(t *testing.T, app core.App, lang string) string {
	t.Helper()

	tag, ok := core.I18n(app).Resolve(lang)
	if !ok {
		t.Fatalf("no catalog for %s", lang)
	}

	return tag
}

func TestBuiltinEmailsFallBackToTheAppDefault(t *testing.T) {
	t.Parallel()

	// no preference on the record: the app default language
	app, user := membersApp(t, true)
	app.Settings().Meta.DefaultLanguage = "fr"

	if err := mails.SendRecordPasswordReset(app, user); err != nil {
		t.Fatal(err)
	}

	want := core.I18n(app).T("fr", "email.passwordReset.subject", i18n.Args{"APP_NAME": "acme_test"})
	if got := app.TestMailer.LastMessage().Subject; got != want || want == "Reset your acme_test password" {
		t.Fatalf("subject %q, want the French %q", got, want)
	}

	// a collection without the language field: also the default
	app2, user2 := membersApp(t, false)
	app2.Settings().Meta.DefaultLanguage = "es"

	if err := mails.SendRecordOTP(app2, user2, "id", "123456"); err != nil {
		t.Fatal(err)
	}

	m := app2.TestMailer.LastMessage()
	if strings.Contains(m.HTML, "one-time password") || !strings.Contains(m.HTML, "123456") {
		t.Fatalf("unexpected OTP email %s", m.HTML)
	}

	// an unknown preference is skipped, not an error
	app3, user3 := membersApp(t, true)
	user3.Set("lang", "tlh")

	if err := mails.SendRecordAuthAlert(app3, user3, "Chrome"); err != nil {
		t.Fatal(err)
	}

	if app3.TestMailer.LastMessage().Subject != "Login from a new location" {
		t.Fatalf("an unknown language must give English, got %q", app3.TestMailer.LastMessage().Subject)
	}
}

func TestTheConfiguredLanguageFieldIsUsed(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	app.Settings().Meta.LanguageField = "locale"

	col := core.NewAuthCollection("people")
	col.Fields.Add(&core.TextField{Name: "locale"}, &core.TextField{Name: "lang"})

	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	user := core.NewRecord(col)
	user.SetEmail("p@example.com")
	user.SetPassword("1234567890")
	user.Set("locale", "ko")
	user.Set("lang", "ar") // ignored: the setting names another field

	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}

	if got := core.RecordLanguage(app, user); got != "ko" {
		t.Fatalf("got %q", got)
	}

	if err := mails.SendRecordVerification(app, user); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(app.TestMailer.LastMessage().HTML, `dir="rtl"`) || !strings.Contains(app.TestMailer.LastMessage().HTML, `lang="ko"`) {
		t.Fatalf("expected Korean, got %s", app.TestMailer.LastMessage().HTML)
	}
}

func TestCustomizedTemplatesAreNeverLocalized(t *testing.T) {
	t.Parallel()

	app, user := membersApp(t, true)
	user.Set("lang", "es")

	col := user.Collection()
	col.VerificationTemplate = core.EmailTemplate{Subject: "Welcome to {APP_NAME}", Body: "<p>Custom {TOKEN}</p>"}

	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	user, _ = app.FindRecordById("members", user.Id)
	user.Set("lang", "es")

	if err := mails.SendRecordVerification(app, user); err != nil {
		t.Fatal(err)
	}

	m := app.TestMailer.LastMessage()
	if m.Subject != "Welcome to acme_test" || !strings.Contains(m.HTML, "<p>Custom ") || strings.Contains(m.HTML, `lang="es"`) {
		t.Fatalf("an admin's template must be sent as it is: %q %s", m.Subject, m.HTML)
	}

	// the fixture "users" collection carries customized templates too
	fixture, _ := app.FindFirstRecordByData("users", "email", "test@example.com")
	if err := mails.SendRecordPasswordReset(app, fixture); err != nil {
		t.Fatal(err)
	}
}

func TestOverrideDirectoryReplacesStrings(t *testing.T) {
	t.Parallel()

	app, user := membersApp(t, true)
	user.Set("lang", "es")

	core.I18n(app).Override("es", map[string]string{"email.verification.subject": "Bienvenido a {APP_NAME}"})

	if err := mails.SendRecordVerification(app, user); err != nil {
		t.Fatal(err)
	}

	if got := app.TestMailer.LastMessage().Subject; got != "Bienvenido a acme_test" {
		t.Fatalf("got %q", got)
	}
}
