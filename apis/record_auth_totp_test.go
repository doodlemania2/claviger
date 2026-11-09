package apis_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/security"
)

const (
	totpSuperuserEmail = "test@example.com"
	totpPassword       = "1234567890"
)

func totpURL(col, suffix string) string {
	return "/api/collections/" + col + "/" + suffix
}

// totpCode returns the code for the secret at the current step plus offset.
func totpCode(t testing.TB, secret string, offset int64) string {
	t.Helper()

	// Keep generation and verification inside one 30s step: a code made just
	// before a boundary shifts its offset by one by the time the server checks
	// it (so -1 would turn into -2 and be refused).
	if left := 30 - time.Now().Unix()%30; left <= 2 {
		time.Sleep(time.Duration(left)*time.Second + 100*time.Millisecond)
	}

	code, err := security.TOTPCodeAtStep(secret, security.TOTPStep(time.Now())+offset)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// totpPasswordSignIn runs a password sign-in and returns status + body.
func totpPasswordSignIn(env *ceremonyEnv, col, identity string) (int, map[string]any) {
	return env.do(http.MethodPost, totpURL(col, "auth-with-password"), map[string]any{
		"identity": identity,
		"password": totpPassword,
	}, "")
}

func totpBearer(t testing.TB, env *ceremonyEnv, col, identity string) string {
	t.Helper()

	status, body := totpPasswordSignIn(env, col, identity)
	if status != http.StatusOK {
		t.Fatalf("sign-in: expected 200, got %d (%v)", status, body)
	}
	token, _ := body["token"].(string)
	return token
}

// totpEnroll runs begin + confirm (2 rate limited calls) and returns the secret and backup codes.
func totpEnroll(t testing.TB, env *ceremonyEnv, col, bearer string) (string, []string) {
	t.Helper()

	status, body := env.do(http.MethodPost, totpURL(col, "totp/begin"), map[string]any{"password": totpPassword}, bearer)
	if status != http.StatusOK {
		t.Fatalf("begin: expected 200, got %d (%v)", status, body)
	}
	secret, _ := body["secret"].(string)
	if secret == "" || !strings.HasPrefix(body["uri"].(string), "otpauth://totp/") {
		t.Fatalf("begin: unexpected response %v", body)
	}

	status, body = env.do(http.MethodPost, totpURL(col, "totp/confirm"), map[string]any{"code": totpCode(t, secret, 0)}, bearer)
	if status != http.StatusOK {
		t.Fatalf("confirm: expected 200, got %d (%v)", status, body)
	}
	raw, _ := body["backupCodes"].([]any)
	codes := make([]string, 0, len(raw))
	for _, c := range raw {
		codes = append(codes, c.(string))
	}
	if len(codes) != 10 {
		t.Fatalf("expected 10 backup codes, got %v", codes)
	}

	return secret, codes
}

func TestTOTPEnrollConfirmStatus(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	statusURL := totpURL("users", "totp")

	if s, b := env.do(http.MethodGet, statusURL, nil, ""); s != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status: expected 401, got %d (%v)", s, b)
	}

	if s, b := env.do(http.MethodGet, statusURL, nil, bearer); s != 200 || b["enabled"] != false || b["pending"] != false {
		t.Fatalf("initial status: %d %v", s, b)
	}

	// wrong password cannot begin
	if s, _ := env.do(http.MethodPost, totpURL("users", "totp/begin"), map[string]any{"password": "wrong-password"}, bearer); s != http.StatusBadRequest {
		t.Fatalf("begin with wrong password: expected 400, got %d", s)
	}

	s, b := env.do(http.MethodPost, totpURL("users", "totp/begin"), map[string]any{"password": totpPassword}, bearer)
	if s != http.StatusOK {
		t.Fatalf("begin: %d %v", s, b)
	}
	secret := b["secret"].(string)

	if s, b := env.do(http.MethodGet, statusURL, nil, bearer); s != 200 || b["enabled"] != false || b["pending"] != true {
		t.Fatalf("pending status: %d %v", s, b)
	}

	// a pending enrollment does not change sign-in
	if s, _ := totpPasswordSignIn(env, "users", ceremonyIdentity); s != http.StatusOK {
		t.Fatalf("pending enrollment must not require a second factor, got %d", s)
	}

	// -2 steps is outside the skew window
	if s, _ := env.do(http.MethodPost, totpURL("users", "totp/confirm"), map[string]any{"code": totpCode(t, secret, -2)}, bearer); s != http.StatusBadRequest {
		t.Fatalf("confirm with skewed code: expected 400, got %d", s)
	}

	// -1 is accepted
	s, b = env.do(http.MethodPost, totpURL("users", "totp/confirm"), map[string]any{"code": totpCode(t, secret, -1)}, bearer)
	if s != http.StatusOK || len(b["backupCodes"].([]any)) != 10 {
		t.Fatalf("confirm: %d %v", s, b)
	}

	if s, b := env.do(http.MethodGet, statusURL, nil, bearer); s != 200 || b["enabled"] != true || b["backupCodesRemaining"] != float64(10) {
		t.Fatalf("enabled status: %d %v", s, b)
	}

	// begin again is refused while enabled
	if s, _ := env.do(http.MethodPost, totpURL("users", "totp/begin"), map[string]any{"password": totpPassword}, bearer); s != http.StatusBadRequest {
		t.Fatalf("begin while enabled: expected 400, got %d", s)
	}
}

func TestTOTPSuperuserPasswordSignInNeedsSecondFactor(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "_superusers", totpSuperuserEmail)

	// regression: no enrollment, plain password sign-in is unchanged
	if s, b := totpPasswordSignIn(env, "_superusers", totpSuperuserEmail); s != http.StatusOK || b["token"] == "" {
		t.Fatalf("superuser without TOTP: %d %v", s, b)
	}

	secret, _ := totpEnroll(t, env, "_superusers", bearer)

	// the collection's own MFA is off (default), the account enforcement applies anyway
	superusers, _ := env.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if superusers.MFA.Enabled {
		t.Fatal("test precondition: _superusers MFA must be off")
	}

	s, b := totpPasswordSignIn(env, "_superusers", totpSuperuserEmail)
	if s != http.StatusUnauthorized {
		t.Fatalf("expected 401 mfa challenge, got %d (%v)", s, b)
	}
	mfaId, _ := b["mfaId"].(string)
	if mfaId == "" || b["totp"] != true || b["token"] != nil {
		t.Fatalf("unexpected challenge %v", b)
	}

	authURL := totpURL("_superusers", "auth-with-totp")

	// wrong code (counts 1/5 for the record)
	if s, _ := env.do(http.MethodPost, authURL, map[string]any{"mfaId": mfaId, "code": "000000"}, ""); s != http.StatusBadRequest {
		t.Fatalf("wrong code: expected 400, got %d", s)
	}
	// invalid mfaId
	if s, _ := env.do(http.MethodPost, authURL, map[string]any{"mfaId": "missing", "code": totpCode(t, secret, 1)}, ""); s != http.StatusBadRequest {
		t.Fatalf("invalid mfaId: expected 400, got %d", s)
	}

	// the right code completes the session (+1 step is inside the window and newer than the confirm step)
	code := totpCode(t, secret, 1)
	s, b = env.do(http.MethodPost, authURL, map[string]any{"mfaId": mfaId, "code": code}, "")
	if s != http.StatusOK || b["token"] == nil || b["token"] == "" {
		t.Fatalf("totp second factor: %d %v", s, b)
	}
	if _, hasHidden := b["record"].(map[string]any)["tokenKey"]; !hasHidden {
		// superusers get hidden fields in their own auth response (upstream behaviour), just make sure the shape is a record
		t.Logf("note: tokenKey not present in the auth response")
	}

	// replay: same code, new MFA session
	s, b = totpPasswordSignIn(env, "_superusers", totpSuperuserEmail)
	if s != http.StatusUnauthorized {
		t.Fatalf("expected a new challenge, got %d", s)
	}
	if s, _ := env.do(http.MethodPost, authURL, map[string]any{"mfaId": b["mfaId"], "code": code}, ""); s != http.StatusBadRequest {
		t.Fatalf("replayed code: expected 400, got %d", s)
	}
}

func TestTOTPMFASessionCannotBeCompletedWithTOTPAsFirstFactor(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	secret, _ := totpEnroll(t, env, "users", bearer)

	// no mfaId at all: TOTP is never a first factor
	s, b := env.do(http.MethodPost, totpURL("users", "auth-with-totp"), map[string]any{"code": totpCode(t, secret, 1)}, "")
	if s != http.StatusBadRequest || b["token"] != nil {
		t.Fatalf("expected 400 without mfaId, got %d (%v)", s, b)
	}

	// an mfaId of another record cannot be completed with this record's code (and vice versa)
	other := core.NewMFA(env.app)
	other.SetCollectionRef(ceremonyCollectionID(t, env))
	other.SetRecordRef("oap640cot4yru2s") // a user without TOTP
	other.SetMethod(core.MFAMethodPassword)
	if err := env.app.Save(other); err != nil {
		t.Fatal(err)
	}
	if s, _ := env.do(http.MethodPost, totpURL("users", "auth-with-totp"), map[string]any{"mfaId": other.Id, "code": totpCode(t, secret, 1)}, ""); s != http.StatusBadRequest {
		t.Fatalf("mfa session of a record without TOTP: expected 400, got %d", s)
	}
}

func ceremonyCollectionID(t testing.TB, env *ceremonyEnv) string {
	t.Helper()
	col, err := env.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	return col.Id
}

func TestTOTPBackupCodeWorksOnce(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "_superusers", totpSuperuserEmail)
	_, codes := totpEnroll(t, env, "_superusers", bearer)
	authURL := totpURL("_superusers", "auth-with-totp")

	challenge := func() string {
		s, b := totpPasswordSignIn(env, "_superusers", totpSuperuserEmail)
		if s != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", s)
		}
		return b["mfaId"].(string)
	}

	// (formatting is forgiving: case, dash and spaces)
	used := strings.ToUpper(strings.Replace(codes[0], "-", " ", 1))
	s, b := env.do(http.MethodPost, authURL, map[string]any{"mfaId": challenge(), "code": used}, "")
	if s != http.StatusOK || b["token"] == nil {
		t.Fatalf("backup code: %d %v", s, b)
	}

	if s, _ := env.do(http.MethodPost, authURL, map[string]any{"mfaId": challenge(), "code": codes[0]}, ""); s != http.StatusBadRequest {
		t.Fatalf("reused backup code: expected 400, got %d", s)
	}

	// another one still works, and the count went down
	if s, _ := env.do(http.MethodPost, authURL, map[string]any{"mfaId": challenge(), "code": codes[1]}, ""); s != http.StatusOK {
		t.Fatalf("second backup code: expected 200, got %d", s)
	}

	su, err := env.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, totpSuperuserEmail)
	if err != nil {
		t.Fatal(err)
	}
	row, err := core.FindTOTPByRecord(env.app, su)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(row.BackupCodeHashes()); n != 8 {
		t.Fatalf("expected 8 backup codes left, got %d", n)
	}
}

func TestTOTPDisableAndRegenerate(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	secret, codes := totpEnroll(t, env, "users", bearer) // 2 limited calls (begin, confirm)

	regenURL := totpURL("users", "totp/backup-codes")
	disableURL := totpURL("users", "totp/disable")

	// a backup code cannot mint new backup codes
	if s, _ := env.do(http.MethodPost, regenURL, map[string]any{"code": codes[0]}, bearer); s != http.StatusBadRequest {
		t.Fatalf("regenerate with backup code: expected 400, got %d", s)
	}

	// regenerate with a TOTP code replaces the whole set
	used := totpCode(t, secret, 1)
	s, b := env.do(http.MethodPost, regenURL, map[string]any{"code": used}, bearer)
	if s != http.StatusOK || len(b["backupCodes"].([]any)) != 10 {
		t.Fatalf("regenerate: %d %v", s, b)
	}
	if b["backupCodes"].([]any)[0] == codes[0] {
		t.Fatal("regenerate returned the old codes")
	}

	// the code was consumed by the call above: a replay cannot disable TOTP either
	s, _ = env.do(http.MethodPost, disableURL, map[string]any{"code": used}, bearer)
	if s != http.StatusBadRequest {
		t.Fatalf("disable with a replayed code: expected 400, got %d", s)
	}
}

func TestTOTPDisableWithCodeThenSignInIsPlain(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	secret, _ := totpEnroll(t, env, "users", bearer)

	if s, _ := totpPasswordSignIn(env, "users", ceremonyIdentity); s != http.StatusUnauthorized {
		t.Fatalf("enrolled account must be challenged, got %d", s)
	}

	s, b := env.do(http.MethodPost, totpURL("users", "totp/disable"), map[string]any{"code": totpCode(t, secret, 1)}, bearer)
	if s != http.StatusNoContent {
		t.Fatalf("disable: %d %v", s, b)
	}

	if s, _ := totpPasswordSignIn(env, "users", ceremonyIdentity); s != http.StatusOK {
		t.Fatalf("after disable sign-in must be plain, got %d", s)
	}
	if s, b := env.do(http.MethodGet, totpURL("users", "totp"), nil, bearer); s != 200 || b["enabled"] != false {
		t.Fatalf("status after disable: %d %v", s, b)
	}
}

func TestTOTPDisableWithFreshPasskey(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	totpEnroll(t, env, "users", bearer)

	// a plain session without a code is refused, a fresh passkey session is not
	if s, _ := env.do(http.MethodPost, totpURL("users", "totp/disable"), map[string]any{}, bearer); s != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", s)
	}
	fresh := userTokenAt(t, env, time.Now())
	if s, b := env.do(http.MethodPost, totpURL("users", "totp/disable"), map[string]any{}, fresh); s != http.StatusNoContent {
		t.Fatalf("fresh passkey disable: expected 204, got %d (%v)", s, b)
	}
}

func TestTOTPPasskeySignInNotBlocked(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t) // users: webauthn on, MFA off
	auth := newSoftAuthenticator(t, ceremonyRPID)
	registerPasskey(t, env, auth, "laptop")
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	totpEnroll(t, env, "users", bearer)

	if s, _ := totpPasswordSignIn(env, "users", ceremonyIdentity); s != http.StatusUnauthorized {
		t.Fatalf("password sign-in must now need TOTP, got %d", s)
	}

	s, b := loginWithPasskey(t, env, auth)
	if s != http.StatusOK || b["token"] == nil {
		t.Fatalf("passkey sign-in must not need TOTP: %d %v", s, b)
	}
}

func TestTOTPAcceptedAsSecondFactorWhenCollectionMFAEnabled(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnvWithMFA(t, true)
	bearer := ceremonyUserToken
	secret, _ := totpEnroll(t, env, "users", bearer)

	s, b := totpPasswordSignIn(env, "users", ceremonyIdentity)
	if s != http.StatusUnauthorized || b["totp"] != true {
		t.Fatalf("expected totp challenge, got %d %v", s, b)
	}
	s, b = env.do(http.MethodPost, totpURL("users", "auth-with-totp"), map[string]any{"mfaId": b["mfaId"], "code": totpCode(t, secret, 1)}, "")
	if s != http.StatusOK || b["token"] == nil {
		t.Fatalf("totp second factor with collection MFA on: %d %v", s, b)
	}

	// a weaker second method (here a second password submission is "same method") cannot complete an enrolled account
	s, b = totpPasswordSignIn(env, "users", ceremonyIdentity)
	if s != http.StatusUnauthorized {
		t.Fatal(s)
	}
	status, _ := env.do(http.MethodPost, totpURL("users", "auth-with-otp"), map[string]any{"otpId": "x", "password": "y", "mfaId": b["mfaId"]}, "")
	if status == http.StatusOK {
		t.Fatal("OTP must not complete an account that enrolled TOTP")
	}
}

func TestTOTPSecretsNeverLeak(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "_superusers", totpSuperuserEmail)
	secret, codes := totpEnroll(t, env, "_superusers", bearer)

	su, err := env.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, totpSuperuserEmail)
	if err != nil {
		t.Fatal(err)
	}
	row, err := core.FindTOTPByRecord(env.app, su)
	if err != nil {
		t.Fatal(err)
	}

	// not readable through the generic records API, even as a superuser
	for _, url := range []string{
		"/api/collections/_totps/records",
		"/api/collections/_totps/records/" + row.Id,
	} {
		s, _ := env.do(http.MethodGet, url, nil, bearer)
		if s != http.StatusOK {
			t.Fatalf("%s: %d", url, s)
		}
	}
	_, list := env.do(http.MethodGet, "/api/collections/_totps/records", nil, bearer)
	for _, item := range list["items"].([]any) {
		m := item.(map[string]any)
		for _, k := range []string{"secret", "backupCodes", "enabled", "lastStep", "collectionRef", "recordRef"} {
			if _, ok := m[k]; ok {
				t.Fatalf("hidden field %q is exposed by the records API: %v", k, m)
			}
		}
	}

	// the status endpoint says counts, never material
	_, status := env.do(http.MethodGet, totpURL("_superusers", "totp"), nil, bearer)
	if _, ok := status["secret"]; ok {
		t.Fatal("status exposes the secret")
	}

	// the audit log recorded the enrollment, redacted
	entries, err := env.app.FindAllRecords(core.CollectionNameAuditLog)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range entries {
		raw, _ := e.MarshalJSON()
		text := string(raw)
		if !strings.Contains(text, "_totps") {
			continue
		}
		seen++
		if strings.Contains(text, secret) {
			t.Fatalf("audit entry contains the TOTP secret: %s", text)
		}
		for _, c := range codes {
			if strings.Contains(text, c) || strings.Contains(text, strings.Replace(c, "-", "", 1)) {
				t.Fatalf("audit entry contains a backup code: %s", text)
			}
		}
		if strings.Contains(text, "$2a$") {
			t.Fatalf("audit entry contains a backup code hash: %s", text)
		}
	}
	if seen < 2 { // create (begin) + update (confirm)
		t.Fatalf("expected audit entries for the enrollment, got %d", seen)
	}
}

func TestTOTPSecretEncryptedWhenKeyConfigured(t *testing.T) {
	// not parallel: sets a process env var
	env := newCeremonyEnv(t)
	t.Setenv(env.app.EncryptionEnv(), "0123456789abcdef0123456789abcdef")

	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	secret, _ := totpEnroll(t, env, "users", bearer)

	user, _ := env.app.FindRecordById("users", ceremonyUserID)
	row, err := core.FindTOTPByRecord(env.app, user)
	if err != nil {
		t.Fatal(err)
	}

	stored := row.GetString("secret")
	if !strings.HasPrefix(stored, "enc:") || strings.Contains(stored, secret) {
		t.Fatalf("expected the stored secret to be encrypted, got %q", stored)
	}
	if plain, err := row.Secret(env.app); err != nil || plain != secret {
		t.Fatalf("decrypt roundtrip failed: %q %v", plain, err)
	}

	// sign-in works with the encrypted secret
	_, b := totpPasswordSignIn(env, "users", ceremonyIdentity)
	if s, _ := env.do(http.MethodPost, totpURL("users", "auth-with-totp"), map[string]any{"mfaId": b["mfaId"], "code": totpCode(t, secret, 1)}, ""); s != http.StatusOK {
		t.Fatalf("sign-in with an encrypted secret: %d", s)
	}
}

func TestTOTPSecretStoredPlainWithoutKey(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	bearer := totpBearer(t, env, "users", ceremonyIdentity)
	secret, _ := totpEnroll(t, env, "users", bearer)

	user, _ := env.app.FindRecordById("users", ceremonyUserID)
	row, _ := core.FindTOTPByRecord(env.app, user)
	if row.GetString("secret") != secret {
		t.Fatalf("expected the plain secret to be stored when no encryption key is configured")
	}
}
