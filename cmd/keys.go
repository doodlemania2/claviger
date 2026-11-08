package cmd

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/spf13/cobra"
)

// Fork-local (Claviger #75).

// KeysCheckFailedPrefix starts the error of a definitive `keys check` failure.
// scripts and entrypoint.sh match on it to tell it from an unrelated failure
// (an unknown command, a crash), which must never block a rollout.
const KeysCheckFailedPrefix = "keys check: FAILED"

const keysCheckTimeout = 15 * time.Second

// NewKeysCommand creates the `keys` command group (field encryption keys).
func NewKeysCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "keys",
		Short: "Manage the field encryption keys",
	}

	command.AddCommand(keysCheckCommand(app))

	return command
}

func keysCheckCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Checks that the configured key-encryption key can open the field keys",
		Long: "Checks, without starting the app and without writing anything, that the configured\n" +
			"key-encryption key (CLAVIGER_KEK, CLAVIGER_KEK_FILE or the app encryption key)\n" +
			"can unwrap the data keys of the encrypted fields in <dir>/data.db.\n\n" +
			"Exit status 0: the keys are fine, there are no encrypted fields, or the database\n" +
			"could not be inspected (the check fails open on infrastructure problems).\n" +
			"Exit status 1: the key is definitely missing or wrong.\n\n" +
			"entrypoint.sh runs it before asking the previous replica to hand over the data\n" +
			"directory, so a wrong key stops a rollout instead of an outage.",
		Example:      "keys check --dir=/pb_data --encryptionEnv=PB_ENCRYPTION_KEY",
		SilenceUsage: true,
		// the error is printed once by the caller
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			msg, err := CheckFieldKeys(command.Context(), app.DataDir(), app.EncryptionEnv(), os.Getenv)
			if err != nil {
				return err
			}

			fmt.Fprintln(command.OutOrStdout(), "keys check: "+msg)

			return nil
		},
	}
}

type fieldKeysCheckRow struct {
	collectionRef, fieldRef, keyId, kekId, wrapped string
}

// CheckFieldKeys is the implementation of `keys check`.
//
// It opens <dataDir>/data.db read-only and immutable, so it takes no locks and
// writes nothing, which is what makes it safe to run while another replica
// still owns the directory. The price is that it does not see changes that are
// still in the WAL.
//
// It returns a non-nil error only when the key is definitely missing or
// wrong. Every problem with inspecting the database (no file, not readable,
// a torn read, a timeout) is reported in msg and returns a nil error.
func CheckFieldKeys(ctx context.Context, dataDir, appKeyEnv string, getenv func(string) string) (msg string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	ctx, cancel := context.WithTimeout(ctx, keysCheckTimeout)
	defer cancel()

	type result struct {
		rows []fieldKeysCheckRow
		live map[string]string
		skip string
	}

	done := make(chan result, 1)
	go func() {
		rows, live, skip := readFieldKeys(ctx, filepath.Join(dataDir, "data.db"))
		done <- result{rows, live, skip}
	}()

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return "skipped (timed out reading the database; the app will check the keys itself when it starts)", nil
	}

	if res.skip != "" {
		return "skipped (" + res.skip + ")", nil
	}

	var liveRows []fieldKeysCheckRow
	for _, row := range res.rows {
		if _, ok := res.live[row.collectionRef+"|"+row.fieldRef]; ok {
			liveRows = append(liveRows, row)
		}
	}

	if len(liveRows) == 0 {
		return "OK (no encrypted fields)", nil
	}

	src, srcErr := keys.LoadWithOld(getenv, appKeyEnv)
	if srcErr != nil {
		return "", fmt.Errorf("%s: %d encrypted field key(s) exist but the key-encryption key is unusable: %w", KeysCheckFailedPrefix, len(liveRows), srcErr)
	}

	for _, row := range liveRows {
		wrapped, decErr := base64.StdEncoding.DecodeString(row.wrapped)
		if decErr != nil {
			return "", fmt.Errorf("%s: data key %s of %s is corrupted (bad encoding)", KeysCheckFailedPrefix, row.keyId, res.live[row.collectionRef+"|"+row.fieldRef])
		}

		if _, unwrapErr := src.Unwrap(ctx, row.kekId, wrapped); unwrapErr != nil {
			return "", fmt.Errorf("%s: cannot open the data key of encrypted field %s with the %s key source (key id %s; the data was wrapped under %s): %w",
				KeysCheckFailedPrefix, res.live[row.collectionRef+"|"+row.fieldRef], src.Name(), src.KeyID(), row.kekId, unwrapErr)
		}
	}

	return fmt.Sprintf("OK (%d encrypted field key(s) opened with key %s)", len(liveRows), src.KeyID()), nil
}

// readFieldKeys returns the stored data keys and the set of live encrypted
// fields ("collectionId|fieldId" -> "collection.field"). skip is non-empty
// when the database could not be inspected, or holds nothing to check.
func readFieldKeys(ctx context.Context, dbPath string) (rows []fieldKeysCheckRow, live map[string]string, skip string) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, nil, "cannot resolve the database path"
	}

	if _, err := os.Stat(abs); err != nil {
		return nil, nil, "no database at " + abs + " yet"
	}

	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1"}

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, nil, "cannot open the database: " + err.Error()
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	var hasTable int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", core.CollectionNameFieldKeys).Scan(&hasTable); err != nil {
		return nil, nil, "cannot read the database: " + err.Error()
	}
	if hasTable == 0 {
		return nil, map[string]string{}, "" // pre-feature database: nothing to check
	}

	rs, err := db.QueryContext(ctx, "SELECT collectionRef, fieldRef, keyId, kekId, wrapped FROM "+core.CollectionNameFieldKeys)
	if err != nil {
		return nil, nil, "cannot read the field keys: " + err.Error()
	}
	for rs.Next() {
		var r fieldKeysCheckRow
		if err := rs.Scan(&r.collectionRef, &r.fieldRef, &r.keyId, &r.kekId, &r.wrapped); err != nil {
			_ = rs.Close()
			return nil, nil, "cannot read the field keys: " + err.Error()
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return nil, nil, "cannot read the field keys: " + err.Error()
	}
	_ = rs.Close()

	if len(rows) == 0 {
		return nil, map[string]string{}, ""
	}

	cs, err := db.QueryContext(ctx, "SELECT id, name, fields FROM _collections")
	if err != nil {
		return nil, nil, "cannot read the collections: " + err.Error()
	}
	defer cs.Close()

	live = map[string]string{}
	for cs.Next() {
		var id, name, fieldsJSON string
		if err := cs.Scan(&id, &name, &fieldsJSON); err != nil {
			return nil, nil, "cannot read the collections: " + err.Error()
		}

		var fields []struct {
			Id   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
			return nil, nil, "cannot parse the fields of collection " + name
		}

		for _, f := range fields {
			if core.IsEncryptedFieldType(f.Type) {
				live[id+"|"+f.Id] = name + "." + f.Name
			}
		}
	}
	if err := cs.Err(); err != nil {
		return nil, nil, "cannot read the collections: " + err.Error()
	}

	return rows, live, ""
}
