package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #82, 82-4): the kiosk principal.
//
// A kiosk is a device, not a user. Its token (clvk_<prefix>_<secret>) is shown
// once at registration and stored as a hash. It is read from the
// X-Kiosk-Token header by the /api/checkin routes only, and checked against
// the database on every request, so revoking a kiosk is effective at once. It
// is not an Authorization token: the records API, realtime and every other
// route see a guest.
//
// A guardian identifies themselves at the kiosk by a handoff: on their phone,
// with a fresh passkey, they ask for a handoff for one kiosk and one household
// (valid two minutes, usable once). The kiosk exchanges it for a kiosk session
// of the org's length (5 minutes by default), a signed token that names the
// kiosk, the household and the guardian. The server enforces the lifetime.

const (
	KioskTokenHeader   = "X-Kiosk-Token"
	KioskSessionHeader = "X-Kiosk-Session"

	kioskHandoffTTL = 2 * time.Minute

	kioskTokenLastSeenInterval = time.Minute
)

// ErrInvalidKioskToken is returned for every failed kiosk authentication.
var ErrInvalidKioskToken = errors.New("the kiosk token is invalid or revoked")

// IsKioskToken reports whether the value looks like a kiosk token.
func IsKioskToken(token string) bool { return strings.HasPrefix(token, KioskTokenPrefix) }

func parseKioskToken(token string) (prefix, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, KioskTokenPrefix)
	if !found {
		return "", "", false
	}

	prefix, secret, found = strings.Cut(rest, "_")
	if !found || len(prefix) != apiKeyPrefixLen || len(secret) != apiKeySecretLen ||
		!apiKeyPartRegex.MatchString(prefix) || !apiKeyPartRegex.MatchString(secret) {
		return "", "", false
	}

	return prefix, secret, true
}

func generateKioskToken() (token, prefix, hash string, err error) {
	pb := make([]byte, 5)
	sb := make([]byte, 32)

	if _, err = rand.Read(pb); err != nil {
		return "", "", "", err
	}

	if _, err = rand.Read(sb); err != nil {
		return "", "", "", err
	}

	prefix = strings.ToLower(apiKeyEncoding.EncodeToString(pb))
	secret := strings.ToLower(apiKeyEncoding.EncodeToString(sb))

	return KioskTokenPrefix + prefix + "_" + secret, prefix, HashAPIKeySecret(secret), nil
}

// RegisterKiosk creates a kiosk and returns its token, which is not stored and
// cannot be shown again. The caller (the route) has checked that admin is an
// owner or admin of the org and holds a fresh passkey.
func RegisterKiosk(app App, ctx context.Context, org, admin *Record, name string, allowedEvents []string) (*Record, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameKiosks)
	if err != nil {
		return nil, "", orgErr(404, "Kiosks are not available.")
	}

	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, "", orgErr(400, "A name of up to 100 characters is required.")
	}

	events := []string{}

	for _, id := range allowedEvents {
		ev, err := app.FindRecordById(CheckinEventsCollection, id)
		if err != nil || ev.GetString("org") != org.Id {
			return nil, "", orgErr(400, "An allowed event does not belong to this org.")
		}

		events = append(events, ev.Id)
	}

	token, prefix, hash, err := generateKioskToken()
	if err != nil {
		return nil, "", err
	}

	rec := NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("name", name)
	rec.Set("prefix", prefix)
	rec.Set("deviceTokenHash", hash)
	rec.Set("registeredBy", admin.Id)
	rec.Set("status", KioskActive)
	rec.Set("allowedEvents", events)

	err = app.RunInTransaction(func(txApp App) error {
		if err := txApp.SaveWithContext(ctx, rec); err != nil {
			return wrapOrgSaveErr(err)
		}

		return writeKioskAudit(txApp, ctx, AuditActionKioskCreate, rec)
	})
	if err != nil {
		return nil, "", err
	}

	return rec, token, nil
}

// RevokeKiosk ends a kiosk. It stays in the table as evidence.
func RevokeKiosk(app App, ctx context.Context, org, by *Record, id string) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	rec, err := app.FindRecordById(CollectionNameKiosks, id)
	if err != nil || rec.GetString("org") != org.Id {
		return nil, orgErr(404, "No such kiosk.")
	}

	if rec.GetString("status") == KioskRevoked {
		return rec, nil
	}

	rec.Set("status", KioskRevoked)
	rec.Set("revokedBy", by.Id)
	rec.Set("revokedAt", types.NowDateTime())

	err = app.RunInTransaction(func(txApp App) error {
		if err := txApp.SaveWithContext(ctx, rec); err != nil {
			return wrapOrgSaveErr(err)
		}

		return writeKioskAudit(txApp, ctx, AuditActionKioskRevoke, rec)
	})
	if err != nil {
		return nil, err
	}

	return rec, nil
}

func writeKioskAudit(app App, ctx context.Context, action string, rec *Record) error {
	if !auditAvailable(app) {
		return nil
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   rec.Collection().Id,
		CollectionName: CollectionNameKiosks,
		RecordId:       rec.Id,
		ChangedFields:  []string{},
		Meta:           map[string]any{"orgId": rec.GetString("org"), "kiosk": rec.Id},
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}

// AuthenticateKiosk resolves the token to its active kiosk and org. Every
// failure is ErrInvalidKioskToken, so neither the error nor the timing says
// which part was wrong. The row is read on every call: nothing is cached.
func AuthenticateKiosk(app App, token string) (kiosk, org *Record, err error) {
	prefix, secret, ok := parseKioskToken(token)
	if !ok || !KiosksAvailable(app) {
		return nil, nil, ErrInvalidKioskToken
	}

	given := HashAPIKeySecret(secret)

	kiosk, err = app.FindFirstRecordByData(CollectionNameKiosks, "prefix", prefix)
	if err != nil {
		subtle.ConstantTimeCompare([]byte(given), []byte(given))

		return nil, nil, ErrInvalidKioskToken
	}

	if subtle.ConstantTimeCompare([]byte(given), []byte(kiosk.GetString("deviceTokenHash"))) != 1 ||
		kiosk.GetString("status") != KioskActive {
		return nil, nil, ErrInvalidKioskToken
	}

	org, err = app.FindRecordById(CollectionNameOrgs, kiosk.GetString("org"))
	if err != nil || org.GetBool("archived") {
		return nil, nil, ErrInvalidKioskToken
	}

	touchKioskLastSeen(app, kiosk)

	return kiosk, org, nil
}

func touchKioskLastSeen(app App, kiosk *Record) {
	now := time.Now().UTC()
	cutoff := now.Add(-kioskTokenLastSeenInterval)

	if last := kiosk.GetDateTime("lastSeen"); !last.IsZero() && last.Time().After(cutoff) {
		return
	}

	_, err := app.NonconcurrentDB().NewQuery(
		"UPDATE {{" + CollectionNameKiosks + "}} SET [[lastSeen]] = {:now} WHERE [[id]] = {:id}",
	).Bind(dbx.Params{"now": now.Format(types.DefaultDateLayout), "id": kiosk.Id}).Execute()
	if err != nil {
		app.Logger().Warn("Failed to update the kiosk last seen time", "kiosk", kiosk.Id, "error", err.Error())
	}
}

// KioskMayServe reports whether the kiosk may serve the event: the event is of
// the kiosk's org, and in its allowed list when it has one.
func KioskMayServe(kiosk, event *Record) bool {
	if kiosk == nil || event == nil || kiosk.GetString("org") != event.GetString("org") {
		return false
	}

	var allowed []string

	_ = kiosk.UnmarshalJSONField("allowedEvents", &allowed)

	return len(allowed) == 0 || slices.Contains(allowed, event.Id)
}

// ---- handoff and session tokens ----------------------------------------------

const (
	kioskTokenHandoff = "handoff"
	kioskTokenSession = "session"
)

// KioskClaims is the content of a handoff or a kiosk session token.
type KioskClaims struct {
	Type      string `json:"t"`
	Kiosk     string `json:"k"`
	Household string `json:"h"`
	User      string `json:"u"`
	Expires   int64  `json:"x"`
	Nonce     string `json:"n"`
}

func signKioskClaims(app App, c KioskClaims) (string, error) {
	key, err := checkinKey(app)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	c.Nonce = base64.RawURLEncoding.EncodeToString(nonce)

	raw, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(raw)

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("kiosk-token\x00" + payload))

	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyKioskClaims(app App, token, typ string) (*KioskClaims, error) {
	key, err := checkinKey(app)
	if err != nil {
		return nil, err
	}

	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return nil, ErrInvalidKioskToken
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("kiosk-token\x00" + payload))

	given, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(given, mac.Sum(nil)) {
		return nil, ErrInvalidKioskToken
	}

	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrInvalidKioskToken
	}

	var c KioskClaims
	if err := json.Unmarshal(raw, &c); err != nil || c.Type != typ || time.Now().Unix() >= c.Expires {
		return nil, ErrInvalidKioskToken
	}

	return &c, nil
}

// IssueKioskHandoff is called with a user who made a fresh passkey assertion:
// it gives them a two-minute, single-use handoff for one kiosk and household.
// The user must be an active guardian of the household, and the kiosk must be
// active in the household's org.
func IssueKioskHandoff(app App, user *Record, householdId, kioskId string) (string, time.Time, error) {
	h, err := app.FindRecordById(CollectionNameHouseholds, householdId)
	if err != nil || CheckinGuardianMember(app, h.Id, user.Id) == nil {
		return "", time.Time{}, orgErr(403, "Only a guardian of the household can check in.")
	}

	k, err := app.FindRecordById(CollectionNameKiosks, kioskId)
	if err != nil || k.GetString("org") != h.GetString("org") || k.GetString("status") != KioskActive {
		return "", time.Time{}, orgErr(404, "No such kiosk.")
	}

	exp := time.Now().Add(kioskHandoffTTL)

	tok, err := signKioskClaims(app, KioskClaims{Type: kioskTokenHandoff, Kiosk: k.Id, Household: h.Id, User: user.Id, Expires: exp.Unix()})
	if err != nil {
		return "", time.Time{}, orgErr(503, err.Error())
	}

	return tok, exp, nil
}

var (
	kioskHandoffMu   sync.Mutex
	kioskHandoffUsed = map[string]int64{}
)

// ExchangeKioskHandoff turns a handoff into a kiosk session of the org's
// length. The handoff works once, for the kiosk it was issued for (single
// process memory: with several replicas the second use is not caught, but the
// two-minute lifetime still holds).
func ExchangeKioskHandoff(app App, kiosk, org *Record, handoff string) (string, time.Time, error) {
	c, err := verifyKioskClaims(app, handoff, kioskTokenHandoff)
	if err != nil || c.Kiosk != kiosk.Id {
		return "", time.Time{}, orgErr(403, "The handoff is invalid or has expired.")
	}

	now := time.Now().Unix()

	kioskHandoffMu.Lock()

	for n, x := range kioskHandoffUsed {
		if x <= now {
			delete(kioskHandoffUsed, n)
		}
	}

	if _, used := kioskHandoffUsed[c.Nonce]; used {
		kioskHandoffMu.Unlock()

		return "", time.Time{}, orgErr(403, "The handoff is invalid or has expired.")
	}

	kioskHandoffUsed[c.Nonce] = c.Expires
	kioskHandoffMu.Unlock()

	// the guardian must still be one
	if CheckinGuardianMember(app, c.Household, c.User) == nil {
		return "", time.Time{}, orgErr(403, "The handoff is invalid or has expired.")
	}

	exp := time.Now().Add(KioskSessionTTL(org))

	tok, err := signKioskClaims(app, KioskClaims{Type: kioskTokenSession, Kiosk: kiosk.Id, Household: c.Household, User: c.User, Expires: exp.Unix()})
	if err != nil {
		return "", time.Time{}, orgErr(503, err.Error())
	}

	return tok, exp, nil
}

// VerifyKioskSession checks a kiosk session token for the kiosk and returns the
// household and the guardian user it names.
func VerifyKioskSession(app App, kiosk *Record, token string) (*KioskClaims, error) {
	c, err := verifyKioskClaims(app, token, kioskTokenSession)
	if err != nil || c.Kiosk != kiosk.Id {
		return nil, ErrInvalidKioskToken
	}

	return c, nil
}
