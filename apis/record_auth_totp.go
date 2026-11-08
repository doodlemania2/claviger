package apis

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/security"
	"golang.org/x/crypto/bcrypt"
)

// TOTP (authenticator app) second factor
// --------------------------------------
//
// Storage: one row per auth record in the hidden-field system collection
// _totps (see core/totp_model.go).
//
// Sign-in behaviour (see checkMFA in record_helpers.go):
//
//   - An account with an active TOTP enrollment needs a TOTP code, or an
//     unused backup code, after its first factor (password, OAuth2, email
//     OTP) EVEN IF the collection's MFA setting is off. Enforcement is per
//     account and opt-in: nothing changes for an account until it enrolls.
//   - A passkey sign-in counts as strong authentication on its own and is not
//     asked for TOTP (unless the collection's own MFA setting asks for a
//     second factor, which then behaves exactly as upstream).
//   - TOTP is also an accepted second factor wherever the collection's MFA
//     setting asks for one, for accounts that enrolled.
//
// The second factor is submitted to POST /api/collections/{c}/auth-with-totp
// together with the mfaId returned (HTTP 401) by the first factor.

const (
	totpBackupCodeCount  = 10
	totpBackupCodeLength = 10
	// no 0/o/1/i/l: backup codes are typed by hand from a printout
	totpBackupCodeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

	totpRateRule = "@pb_totp_"
)

var totpRateLimit = core.RateLimitRule{MaxRequests: 5, Duration: 180}

func bindTOTPApi(sub *router.RouterGroup[*core.RequestEvent]) {
	sub.POST("/auth-with-totp", recordAuthWithTOTP).Bind(
		collectionPathRateLimit("", "authWithTOTP", "auth"),
	)

	sub.GET("/totp", recordTOTPStatus).Bind(
		collectionPathRateLimit("", "totpStatus"),
		RequireSameCollectionContextAuth(""),
	)
	sub.POST("/totp/begin", recordTOTPBegin).Bind(
		collectionPathRateLimit("", "totpBegin"),
		RequireSameCollectionContextAuth(""),
	)
	sub.POST("/totp/confirm", recordTOTPConfirm).Bind(
		collectionPathRateLimit("", "totpConfirm"),
		RequireSameCollectionContextAuth(""),
	)
	sub.POST("/totp/disable", recordTOTPDisable).Bind(
		collectionPathRateLimit("", "totpDisable"),
		RequireSameCollectionContextAuth(""),
	)
	sub.POST("/totp/backup-codes", recordTOTPRegenerateBackupCodes).Bind(
		collectionPathRateLimit("", "totpBackupCodes"),
		RequireSameCollectionContextAuth(""),
	)
}

type totpCodeForm struct {
	Code string `form:"code" json:"code"`
}

func (form *totpCodeForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.Code, validation.Required, validation.Length(1, 64)),
	)
}

// findOwnTOTP loads the authenticated record's enrollment (nil, nil if none).
func findOwnTOTP(e *core.RequestEvent) (*core.TOTP, error) {
	t, err := core.FindTOTPByRecord(e.App, e.Auth)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, e.InternalServerError("Failed to load the TOTP state.", err)
	}
	return t, nil
}

// GET .../totp
func recordTOTPStatus(e *core.RequestEvent) error {
	t, err := findOwnTOTP(e)
	if err != nil {
		return err
	}

	result := map[string]any{
		"enabled":              false,
		"pending":              false,
		"backupCodesRemaining": 0,
	}
	if t != nil {
		result["enabled"] = t.Enabled()
		result["pending"] = !t.Enabled()
		if t.Enabled() {
			result["backupCodesRemaining"] = len(t.BackupCodeHashes())
		}
	}

	return e.JSON(http.StatusOK, result)
}

type totpBeginForm struct {
	Password string `form:"password" json:"password"`
}

// POST .../totp/begin
//
// Creates (or replaces) the pending enrollment and returns the secret and the
// otpauth:// URI. Nothing is enforced until the first code is confirmed.
//
// Starting an enrollment needs the current password or a fresh passkey
// assertion: a stolen session must not be able to enroll its own authenticator
// and lock the owner out.
func recordTOTPBegin(e *core.RequestEvent) error {
	if err := checkRateLimit(e, totpRateRule+e.Auth.Id, totpRateLimit); err != nil {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	form := &totpBeginForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}

	if !HasFreshPasskey(e, webauthnStepUpMaxAge) && (form.Password == "" || !e.Auth.ValidatePassword(form.Password)) {
		return e.BadRequestError("An error occurred while validating the submitted data.", validation.Errors{
			"password": validation.NewError("validation_invalid_password", "Missing or invalid password."),
		})
	}

	t, err := findOwnTOTP(e)
	if err != nil {
		return err
	}
	if t != nil && t.Enabled() {
		return e.BadRequestError("TOTP is already enabled. Disable it first to enroll a new authenticator.", nil)
	}
	if t == nil {
		t = core.NewTOTP(e.App)
		t.SetCollectionRef(e.Auth.Collection().Id)
		t.SetRecordRef(e.Auth.Id)
	}

	secret := security.GenerateTOTPSecret()
	if err := t.SetSecret(e.App, secret); err != nil {
		return e.InternalServerError("Failed to store the TOTP secret.", err)
	}
	t.SetEnabled(false)
	t.SetLastStep(0)
	t.SetBackupCodeHashes(nil)

	if err := e.App.SaveWithContext(auditContext(e), t); err != nil {
		return firstApiError(err, e.InternalServerError("Failed to store the TOTP secret.", err))
	}

	issuer := e.App.Settings().Meta.AppName
	account := e.Auth.Email()
	if account == "" {
		account = e.Auth.Id
	}

	return e.JSON(http.StatusOK, map[string]any{
		"secret":  secret,
		"uri":     security.TOTPProvisioningURI(issuer, account, secret),
		"issuer":  issuer,
		"account": account,
	})
}

// POST .../totp/confirm
//
// Activates the pending enrollment with the first valid code and returns the
// backup codes. This is the only time they are shown.
func recordTOTPConfirm(e *core.RequestEvent) error {
	if err := checkRateLimit(e, totpRateRule+e.Auth.Id, totpRateLimit); err != nil {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	form := &totpCodeForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	t, err := findOwnTOTP(e)
	if err != nil {
		return err
	}
	if t == nil || t.Enabled() {
		return e.BadRequestError("There is no pending TOTP enrollment.", nil)
	}

	codes, hashes, err := generateBackupCodes(totpBackupCodeCount)
	if err != nil {
		return e.InternalServerError("Failed to generate the backup codes.", err)
	}

	ok, err := consumeTOTPCode(e.App, t, form.Code)
	if err != nil {
		return e.InternalServerError("Failed to verify the code.", err)
	}
	if !ok {
		return e.BadRequestError("Invalid code.", validation.Errors{
			"code": validation.NewError("validation_invalid_totp", "Invalid or expired code."),
		})
	}

	// reload: consumeTOTPCode updated lastStep outside of the model
	if t, err = core.FindTOTPByRecord(e.App, e.Auth); err != nil {
		return e.InternalServerError("Failed to load the TOTP state.", err)
	}
	t.SetEnabled(true)
	t.SetBackupCodeHashes(hashes)
	if err := e.App.SaveWithContext(auditContext(e), t); err != nil {
		return firstApiError(err, e.InternalServerError("Failed to activate TOTP.", err))
	}

	return e.JSON(http.StatusOK, map[string]any{
		"enabled":     true,
		"backupCodes": codes,
	})
}

// POST .../totp/disable
//
// Needs a valid TOTP code (or an unused backup code), or a passkey assertion
// from the last webauthnStepUpMaxAge.
func recordTOTPDisable(e *core.RequestEvent) error {
	if err := checkRateLimit(e, totpRateRule+e.Auth.Id, totpRateLimit); err != nil {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	form := &totpCodeForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}

	t, err := findOwnTOTP(e)
	if err != nil {
		return err
	}
	if t == nil {
		return e.BadRequestError("TOTP is not enabled.", nil)
	}

	if !t.Enabled() {
		// a pending enrollment protects nothing yet, cancel it freely
		if err := e.App.DeleteWithContext(auditContext(e), t); err != nil {
			return e.InternalServerError("Failed to cancel the enrollment.", err)
		}
		return e.NoContent(http.StatusNoContent)
	}

	if !HasFreshPasskey(e, webauthnStepUpMaxAge) {
		if err := form.validate(); err != nil {
			return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
		}

		ok, err := verifySecondFactor(auditContext(e), e.App, t, form.Code, true)
		if err != nil {
			return e.InternalServerError("Failed to verify the code.", err)
		}
		if !ok {
			return e.BadRequestError("Invalid code.", validation.Errors{
				"code": validation.NewError("validation_invalid_totp", "Invalid or expired code."),
			})
		}
	}

	if err := e.App.DeleteWithContext(auditContext(e), t); err != nil {
		return e.InternalServerError("Failed to disable TOTP.", err)
	}

	return e.NoContent(http.StatusNoContent)
}

// POST .../totp/backup-codes
//
// Replaces the whole backup code set (the old codes stop working). Needs a
// valid TOTP code: a backup code cannot mint new backup codes.
func recordTOTPRegenerateBackupCodes(e *core.RequestEvent) error {
	if err := checkRateLimit(e, totpRateRule+e.Auth.Id, totpRateLimit); err != nil {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	form := &totpCodeForm{}
	if err := e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err := form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	t, err := findOwnTOTP(e)
	if err != nil {
		return err
	}
	if t == nil || !t.Enabled() {
		return e.BadRequestError("TOTP is not enabled.", nil)
	}

	ok, err := verifySecondFactor(auditContext(e), e.App, t, form.Code, false)
	if err != nil {
		return e.InternalServerError("Failed to verify the code.", err)
	}
	if !ok {
		return e.BadRequestError("Invalid code.", validation.Errors{
			"code": validation.NewError("validation_invalid_totp", "Invalid or expired code."),
		})
	}

	codes, hashes, err := generateBackupCodes(totpBackupCodeCount)
	if err != nil {
		return e.InternalServerError("Failed to generate the backup codes.", err)
	}

	if t, err = core.FindTOTPByRecord(e.App, e.Auth); err != nil {
		return e.InternalServerError("Failed to load the TOTP state.", err)
	}
	t.SetBackupCodeHashes(hashes)
	if err := e.App.SaveWithContext(auditContext(e), t); err != nil {
		return firstApiError(err, e.InternalServerError("Failed to store the backup codes.", err))
	}

	return e.JSON(http.StatusOK, map[string]any{"backupCodes": codes})
}

type authWithTOTPForm struct {
	MfaId string `form:"mfaId" json:"mfaId"`
	Code  string `form:"code" json:"code"`
}

func (form *authWithTOTPForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(&form.MfaId, validation.Required, validation.Length(1, 255)),
		validation.Field(&form.Code, validation.Required, validation.Length(1, 64)),
	)
}

// POST .../auth-with-totp
//
// Completes an MFA session (the mfaId of the first factor) with a TOTP code or
// an unused backup code. It is never a first factor: without a valid mfaId it
// fails.
func recordAuthWithTOTP(e *core.RequestEvent) error {
	collection, err := findAuthCollection(e)
	if err != nil {
		return err
	}

	form := &authWithTOTPForm{}
	if err = e.BindBody(form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}
	if err = form.validate(); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	// generic message on purpose (basic enumeration protection, same as OTP)
	const invalid = "Invalid or expired MFA session."

	mfa, err := e.App.FindMFAById(form.MfaId)
	if err != nil {
		return e.BadRequestError(invalid, err)
	}
	if mfa.CollectionRef() != collection.Id {
		return e.BadRequestError(invalid, errors.New("the MFA session is for a different collection"))
	}
	if mfa.HasExpired(mfaSessionDuration(collection)) {
		return e.BadRequestError(invalid, errors.New("the MFA session is expired"))
	}
	if mfa.Method() == core.MFAMethodTOTP {
		return e.BadRequestError(invalid, errors.New("invalid MFA session"))
	}

	record, err := e.App.FindRecordById(collection, mfa.RecordRef())
	if err != nil {
		return e.BadRequestError(invalid, err)
	}

	t, err := core.FindTOTPByRecord(e.App, record)
	if err != nil || !t.Enabled() {
		return e.BadRequestError(invalid, errors.New("TOTP is not enabled for the record"))
	}

	if err := checkRateLimit(e, totpRateRule+record.Id, totpRateLimit); err != nil {
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	ok, err := verifySecondFactor(auditContextAs(e, record), e.App, t, form.Code, true)
	if err != nil {
		return e.InternalServerError("Failed to verify the code.", err)
	}
	if !ok {
		return e.BadRequestError("Invalid code.", errors.New("invalid TOTP or backup code"))
	}

	return RecordAuthResponse(e, record, core.MFAMethodTOTP, nil)
}

// -------------------------------------------------------------------

// mfaRequiredBody is the body of the 401 "second factor needed" response.
// "totp": true tells the client that the account's second factor is an
// authenticator app (or backup code), not the collection's other methods.
func mfaRequiredBody(app core.App, authRecord *core.Record, mfaId string) map[string]any {
	result := map[string]any{"mfaId": mfaId}

	if on, err := core.HasActiveTOTP(app, authRecord); err == nil && on {
		result["totp"] = true
	}

	return result
}

// mfaSessionDuration is the lifetime of a pending MFA session. It guards
// against a zero duration (MFA disabled collections are not validated) which
// would expire every session instantly and lock the account out.
func mfaSessionDuration(collection *core.Collection) time.Duration {
	d := collection.MFA.DurationTime()
	if d <= 0 {
		d = 10 * time.Minute
	}
	return d
}

// verifySecondFactor checks a TOTP code or (allowBackup) a backup code for the
// enrollment and consumes it. A 6 digit code is a TOTP code, anything else is
// treated as a backup code.
func verifySecondFactor(ctx context.Context, app core.App, t *core.TOTP, code string, allowBackup bool) (bool, error) {
	trimmed := strings.ReplaceAll(strings.TrimSpace(code), " ", "")

	if len(trimmed) == security.TOTPDigits && isAllDigits(trimmed) {
		return consumeTOTPCode(app, t, trimmed)
	}

	if !allowBackup {
		return false, nil
	}

	return consumeBackupCode(ctx, app, t, code)
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// consumeTOTPCode validates the code and atomically advances the replay
// counter: of two concurrent submissions of the same code only one wins.
func consumeTOTPCode(app core.App, t *core.TOTP, code string) (bool, error) {
	secret, err := t.Secret(app)
	if err != nil {
		return false, err
	}

	step, ok, err := security.ValidateTOTP(secret, code, time.Now(), t.LastStep())
	if err != nil || !ok {
		return false, err
	}

	res, err := app.DB().
		NewQuery("UPDATE {{" + core.CollectionNameTOTPs + "}} SET [[lastStep]] = {:step} WHERE [[id]] = {:id} AND [[lastStep]] < {:step}").
		Bind(map[string]any{"step": step, "id": t.Id}).
		Execute()
	if err != nil {
		return false, err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return n == 1, nil
}

func normalizeBackupCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	return strings.ReplaceAll(code, " ", "")
}

func generateBackupCodes(n int) (plain []string, hashes []string, err error) {
	for i := 0; i < n; i++ {
		raw := security.RandomStringWithAlphabet(totpBackupCodeLength, totpBackupCodeAlphabet)

		hash, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, err
		}

		half := totpBackupCodeLength / 2
		plain = append(plain, raw[:half]+"-"+raw[half:])
		hashes = append(hashes, string(hash))
	}

	return plain, hashes, nil
}

// consumeBackupCode checks the code against the unused backup code hashes and
// removes the match, in one transaction so a code cannot be used twice.
func consumeBackupCode(ctx context.Context, app core.App, t *core.TOTP, code string) (bool, error) {
	normalized := normalizeBackupCode(code)
	if len(normalized) != totpBackupCodeLength {
		return false, nil
	}

	var used bool

	err := app.RunInTransaction(func(txApp core.App) error {
		rec, err := txApp.FindRecordById(core.CollectionNameTOTPs, t.Id)
		if err != nil {
			return err
		}
		fresh := &core.TOTP{}
		fresh.SetProxyRecord(rec)

		hashes := fresh.BackupCodeHashes()
		for i, h := range hashes {
			if bcrypt.CompareHashAndPassword([]byte(h), []byte(normalized)) != nil {
				continue
			}

			remaining := append(append([]string{}, hashes[:i]...), hashes[i+1:]...)
			fresh.SetBackupCodeHashes(remaining)
			if err := txApp.SaveWithContext(ctx, fresh); err != nil {
				return err
			}

			used = true
			break
		}

		return nil
	})

	return used, err
}
