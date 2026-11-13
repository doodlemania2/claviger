package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// Fork-local (Claviger #87, 87-3): the optional hash chain of a register.
//
// When a register has chain on, finalizing an entry gives it the next number
// (seq, from 1) and a hash:
//
//	hash(n) = hex(SHA-256( hash(n-1) + "\n" + canonical(entry n) ))      hash(0) = ""
//
// canonical(entry) is the JSON of an object (keys sorted, as encoding/json
// writes a map) holding every field of the entry whose value is not empty,
// except: the autodate fields, the mutable fields (default "restricted"), the
// pending-request fields and the hash itself. The id, the finalize time, the
// signers and seq are part of it. Empty values are left out, so adding a new
// field to the collection later does not change the hash of old entries.
//
// A chain does not prove who wrote an entry and it cannot see that the last
// entries were removed; keep the head (see VerifyRegister) somewhere else if
// that matters. It detects an entry that was changed, removed from the
// middle, inserted or renumbered outside the application.

// RegisterBreak is one finding of a verification.
type RegisterBreak struct {
	Collection string `json:"collection"`
	Entry      string `json:"entry"`
	Seq        int    `json:"seq"`
	Problem    string `json:"problem"`
}

// RegisterVerification is the result of verifying one register.
type RegisterVerification struct {
	Collection string          `json:"collection"`
	Chain      bool            `json:"chain"`
	Finalized  int             `json:"finalized"`
	HeadSeq    int             `json:"headSeq"`
	HeadHash   string          `json:"headHash"`
	Breaks     []RegisterBreak `json:"breaks"`
}

// OK is true when no break was found.
func (v *RegisterVerification) OK() bool { return len(v.Breaks) == 0 }

func registerCanonical(rec *Record, cfg *PermanentConfig, withFinal bool) []byte {
	skip := []string{"hash", "pendingBy", "pendingAt", "pendingHash"}
	if !withFinal {
		skip = append(skip, cfg.FinalizeField, "finalizedBy", "secondSigner", "seq")
	}

	out := map[string]any{}

	for _, f := range rec.Collection().Fields {
		name := f.GetName()

		if _, ok := f.(*AutodateField); ok || slices.Contains(skip, name) || cfg.isMutable(name) {
			continue
		}

		if v := registerFieldValue(rec, name); !registerIsZero(v) {
			out[name] = v
		}
	}

	b, _ := json.Marshal(out)

	return b
}

// registerContentHash identifies the content of a draft (what a second signer
// is asked to confirm).
func registerContentHash(rec *Record, cfg *PermanentConfig) string {
	sum := sha256.Sum256(registerCanonical(rec, cfg, false))

	return hex.EncodeToString(sum[:])
}

func registerChainHash(prev string, rec *Record, cfg *PermanentConfig) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte("\n"))
	h.Write(registerCanonical(rec, cfg, true))

	return hex.EncodeToString(h.Sum(nil))
}

// registerChainTail returns the number and hash of the last chained entry.
func registerChainTail(app App, cfg *PermanentConfig) (int, string, error) {
	rows, err := app.FindRecordsByFilter(cfg.Collection, "seq > 0", "-seq", 1, 0)
	if err != nil {
		return 0, "", err
	}

	if len(rows) == 0 {
		return 0, "", nil
	}

	return rows[0].GetInt("seq"), rows[0].GetString("hash"), nil
}

// VerifyRegister recomputes the chain of one register and reports the breaks.
// A register without a chain reports its count only.
func VerifyRegister(app App, cfg *PermanentConfig) (*RegisterVerification, error) {
	res := &RegisterVerification{Collection: cfg.Collection, Chain: cfg.Chain, Breaks: []RegisterBreak{}}

	col, err := app.FindCollectionByNameOrId(cfg.Collection)
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", cfg.Collection, err)
	}

	sort := cfg.FinalizeField + ",id"
	if cfg.Chain {
		sort = "seq,id"
	}

	rows, err := app.FindRecordsByFilter(col, fmt.Sprintf("%s != ''", cfg.FinalizeField), sort, 0, 0)
	if err != nil {
		return nil, err
	}

	res.Finalized = len(rows)

	if !cfg.Chain {
		return res, nil
	}

	expected, prev := 1, ""

	for _, r := range rows {
		seq := r.GetInt("seq")

		brk := func(problem string) {
			res.Breaks = append(res.Breaks, RegisterBreak{Collection: cfg.Collection, Entry: r.Id, Seq: seq, Problem: problem})
		}

		switch {
		case seq == 0:
			brk("final entry without a chain number")

			continue
		case seq != expected:
			brk(fmt.Sprintf("expected number %d, found %d (an entry was removed, inserted or renumbered)", expected, seq))
		}

		if want := registerChainHash(prev, r, cfg); want != r.GetString("hash") {
			brk("hash does not match: the entry or its predecessor was changed outside the application")
		}

		// the stored hash is the link, so one changed entry is one finding and not a cascade
		expected, prev = seq+1, r.GetString("hash")
		res.HeadSeq, res.HeadHash = seq, prev
	}

	return res, nil
}

// VerifyRegisters verifies every configured register. The error is for a
// register that could not be read; breaks are in the results.
func VerifyRegisters(app App) ([]*RegisterVerification, error) {
	resetPermanentConfigs(app)

	var out []*RegisterVerification

	for _, cfg := range PermanentConfigs(app) {
		v, err := VerifyRegister(app, cfg)
		if err != nil {
			return out, err
		}

		out = append(out, v)
	}

	return out, nil
}
