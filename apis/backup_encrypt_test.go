package apis_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/backupenc"
)

// not parallel: the key source comes from the process environment

const backupEncSuperuserAuth = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"

const backupEncFileToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsImV4cCI6MjUyNDYwNDQ2MSwidHlwZSI6ImZpbGUiLCJjb2xsZWN0aW9uSWQiOiJwYmNfMzE0MjYzNTgyMyJ9.Lupz541xRvrktwkrl55p5pPCF77T69ZRsohsIcb2dxc"

func setBackupEncKEK(t *testing.T, kek string) {
	t.Helper()
	t.Setenv("CLAVIGER_KEK", kek)
	t.Setenv("CLAVIGER_KEK_FILE", "")
	t.Setenv("CLAVIGER_KEK_OLD", "")
	t.Setenv("CLAVIGER_KEK_FILE_OLD", "")
	t.Setenv("CLAVIGER_KEK_OLD_FROM_APP_KEY", "")
	t.Setenv("PB_TEST_ENV", "") // the app encryption key fallback (tests use the "pb_test_env" env name)
}

func newBackupEncKEK(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func enableBackupEncryption(app *tests.TestApp) {
	app.Settings().Backups.Encrypt = true
}

func TestBackupsEncryptedAPI(t *testing.T) {
	setBackupEncKEK(t, newBackupEncKEK(t))

	superuser := map[string]string{"Authorization": backupEncSuperuserAuth}

	// builds an upload body
	upload := func(name string, content []byte) (io.Reader, string) {
		b := new(bytes.Buffer)
		mw := multipart.NewWriter(b)
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(content)
		mw.Close()
		return b, mw.FormDataContentType()
	}

	var encBytes []byte

	scenarios := []tests.ApiScenario{
		{
			Name:    "create stores the name with the .enc suffix when encrypting",
			Method:  http.MethodPost,
			URL:     "/api/backups",
			Body:    strings.NewReader(`{"name":"church.zip"}`),
			Headers: superuser,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableBackupEncryption(app)
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				files, err := getBackupFiles(app)
				if err != nil {
					t.Fatal(err)
				}
				if len(files) != 1 || files[0].Key != "church.zip.enc" {
					t.Fatalf("Expected church.zip.enc, got %v", files)
				}
			},
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{"*": 0, "OnBackupCreate": 1},
		},
		{
			Name:    "create rejects a name whose encrypted file already exists",
			Method:  http.MethodPost,
			URL:     "/api/backups",
			Body:    strings.NewReader(`{"name":"church.zip"}`),
			Headers: superuser,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableBackupEncryption(app)
				if err := app.CreateBackup(context.Background(), "church.zip"); err != nil {
					t.Fatal(err)
				}
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"validation_backup_name_exists"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:    "an explicit .zip.enc name encrypts even with the setting off",
			Method:  http.MethodPost,
			URL:     "/api/backups",
			Body:    strings.NewReader(`{"name":"manual.zip.enc"}`),
			Headers: superuser,
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				files, _ := getBackupFiles(app)
				if len(files) != 1 || files[0].Key != "manual.zip.enc" {
					t.Fatalf("Expected manual.zip.enc, got %v", files)
				}
			},
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{"*": 0, "OnBackupCreate": 1},
		},
		{
			Name:    "with the setting off plain names are unchanged",
			Method:  http.MethodPost,
			URL:     "/api/backups",
			Body:    strings.NewReader(`{"name":"plain.zip"}`),
			Headers: superuser,
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				files, _ := getBackupFiles(app)
				if len(files) != 1 || files[0].Key != "plain.zip" {
					t.Fatalf("Expected plain.zip, got %v", files)
				}
			},
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{"*": 0, "OnBackupCreate": 1},
		},
		{
			Name:    "create fails clearly when the key is missing",
			Method:  http.MethodPost,
			URL:     "/api/backups",
			Body:    strings.NewReader(`{"name":"nokey.zip"}`),
			Headers: superuser,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableBackupEncryption(app)
				t.(*testing.T).Setenv("CLAVIGER_KEK", "")
				t.(*testing.T).Setenv(app.EncryptionEnv(), "")
				app.Store().Remove("__clavigerFieldKeySource__")
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				ensureNoBackups(t, app)
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{"Failed to create backup: ", "CLAVIGER_KEK"},
			ExpectedEvents:  map[string]int{"*": 0, "OnBackupCreate": 1},
		},
		{
			Name:    "list flags the encrypted backups",
			Method:  http.MethodGet,
			URL:     "/api/backups",
			Headers: superuser,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				if err := app.CreateBackup(context.Background(), "a.zip"); err != nil {
					t.Fatal(err)
				}
				enableBackupEncryption(app)
				if err := app.CreateBackup(context.Background(), "b.zip"); err != nil {
					t.Fatal(err)
				}
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"key":"a.zip"`, `"key":"b.zip.enc"`, `"encrypted":true`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:   "download returns the encrypted file",
			Method: http.MethodGet,
			URL:    "/api/backups/dl.zip.enc?token=" + backupEncFileToken,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableBackupEncryption(app)
				if err := app.CreateBackup(context.Background(), "dl.zip"); err != nil {
					t.Fatal(err)
				}
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{backupenc.Magic},
			NotExpectedContent: []string{
				"auxiliary.db",
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "upload accepts an encrypted backup",
			Method: http.MethodPost,
			URL:    "/api/backups/upload",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				enableBackupEncryption(app)
				if err := app.CreateBackup(context.Background(), "src.zip"); err != nil {
					t.Fatal(err)
				}
				encBytes = nil
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				files, _ := getBackupFiles(app)
				found := false
				for _, f := range files {
					if f.Key == "uploaded.zip.enc" {
						found = true
					}
				}
				if !found {
					t.Fatalf("Expected the uploaded encrypted backup, got %v", files)
				}
			},
			ExpectedStatus: 204,
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "upload rejects a .enc file that is not an encrypted backup",
			Method: http.MethodPost,
			URL:    "/api/backups/upload",
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				ensureNoBackups(t, app)
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"validation_invalid_mime_type"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	// the two upload scenarios need a body built before the run
	{
		raw := func() []byte {
			enc, err := encryptedFixture(t)
			if err != nil {
				t.Fatal(err)
			}
			return enc
		}()
		encBytes = raw

		b, ct := upload("uploaded.zip.enc", encBytes)
		scenarios[7].Body = b
		scenarios[7].Headers = map[string]string{"Authorization": backupEncSuperuserAuth, "Content-Type": ct}

		b, ct = upload("fake.zip.enc", []byte("this is not an encrypted backup, just some text"))
		scenarios[8].Body = b
		scenarios[8].Headers = map[string]string{"Authorization": backupEncSuperuserAuth, "Content-Type": ct}
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

// encryptedFixture builds a small valid encrypted backup with the current CLAVIGER_KEK.
func encryptedFixture(t *testing.T) ([]byte, error) {
	app, err := tests.NewTestApp()
	if err != nil {
		return nil, err
	}
	defer app.Cleanup()

	enableBackupEncryption(app)
	if err := app.CreateBackup(context.Background(), "fixture.zip"); err != nil {
		return nil, err
	}

	return readBackupFile(app, "fixture.zip.enc")
}

func readBackupFile(app core.App, name string) ([]byte, error) {
	fsys, err := app.NewBackupsFilesystem()
	if err != nil {
		return nil, err
	}
	defer fsys.Close()

	r, err := fsys.GetReader(name)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return io.ReadAll(r)
}

func TestSettingsEnableBackupEncryption(t *testing.T) {
	kek := newBackupEncKEK(t)
	setBackupEncKEK(t, kek)

	superuser := map[string]string{"Authorization": backupEncSuperuserAuth}

	scenarios := []tests.ApiScenario{
		{
			Name:    "enabling without a key is refused",
			Method:  http.MethodPatch,
			URL:     "/api/settings",
			Body:    strings.NewReader(`{"backups":{"encrypt":true}}`),
			Headers: superuser,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				t.(*testing.T).Setenv("CLAVIGER_KEK", "")
				t.(*testing.T).Setenv(app.EncryptionEnv(), "")
				app.Store().Remove("__clavigerFieldKeySource__")
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if app.Settings().Backups.Encrypt {
					t.Fatal("expected the setting to stay off")
				}
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{"Cannot enable backup encryption", `"validation_backup_encryption_key"`, "CLAVIGER_KEK"},
			ExpectedEvents:  map[string]int{"*": 0, "OnSettingsUpdateRequest": 1},
		},
		{
			Name:    "enabling with a working key is saved",
			Method:  http.MethodPatch,
			URL:     "/api/settings",
			Body:    strings.NewReader(`{"backups":{"encrypt":true}}`),
			Headers: superuser,
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if !app.Settings().Backups.Encrypt {
					t.Fatal("expected the setting to be on")
				}
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"encrypt":true`},
			ExpectedEvents: map[string]int{
				"*":                         0,
				"OnSettingsUpdateRequest":   1,
				"OnModelValidate":           1,
				"OnAuditEntry":              1,
				"OnModelUpdate":             1,
				"OnModelUpdateExecute":      1,
				"OnModelAfterUpdateSuccess": 1,
				"OnSettingsReload":          1,
			},
		},
		{
			Name:    "an unrelated settings change does not check the key",
			Method:  http.MethodPatch,
			URL:     "/api/settings",
			Body:    strings.NewReader(`{"meta":{"appName":"Church"}}`),
			Headers: superuser,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				t.(*testing.T).Setenv("CLAVIGER_KEK", "")
				t.(*testing.T).Setenv(app.EncryptionEnv(), "")
				app.Store().Remove("__clavigerFieldKeySource__")
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"encrypt":false`},
			ExpectedEvents: map[string]int{
				"*":                         0,
				"OnSettingsUpdateRequest":   1,
				"OnModelValidate":           1,
				"OnAuditEntry":              1,
				"OnModelUpdate":             1,
				"OnModelUpdateExecute":      1,
				"OnModelAfterUpdateSuccess": 1,
				"OnSettingsReload":          1,
			},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
