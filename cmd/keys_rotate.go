package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/spf13/cobra"
)

// Fork-local (Claviger #75, M3): the maintenance commands of the field keys.
//
//	keys status                         what exists and what is pending
//	keys rotate [--collection --field]  new data key + re-encrypt the rows (resumable)
//	keys rotate --new-kek / keys rewrap re-wrap the data keys under the primary KEK
//	keys convert --collection [--field] encrypt legacy plaintext values in place
//	keys strict --collection --field    strict mode on (--off: off)
//	keys prune [--collection --field]   delete retired data keys no row uses
//
// The commands open the data directory themselves, so they conflict with a
// running server (the deployment is single-writer). They therefore refuse to
// run when the single-writer lock is held or a handover is in progress, unless
// --offline-ok. The same operations are available inside the serving process
// (POST /api/field-keys/..., Settings > Encryption keys in the dashboard),
// which is the way to rotate a live system.

const (
	// single-writer files, see entrypoint.sh
	singleWriterLockName = ".pb_singlewriter.lock"
	handoverFileName     = ".pb_handover"
)

// ServerLiveReason reports why dataDir looks owned by a running server, or ""
// when nothing indicates one. It relies on the files of the single-writer
// handover protocol (entrypoint.sh): the flock on .pb_singlewriter.lock is held
// by the whole process tree of a replica, and .pb_handover exists while an
// incoming replica waits for the outgoing one.
//
// A server started without the entrypoint takes no lock and cannot be seen
// this way; --offline-ok is the operator's statement that none is running.
func ServerLiveReason(dataDir string) string {
	if _, err := os.Stat(filepath.Join(dataDir, handoverFileName)); err == nil {
		return "a rollout handover is in progress (" + handoverFileName + " exists)"
	}

	held, release, err := tryLockFile(filepath.Join(dataDir, singleWriterLockName))
	if err == nil {
		if held {
			return "the single-writer lock (" + singleWriterLockName + ") is held by a running server"
		}
		release()
	}

	return ""
}

// acquireOfflineLock holds the single-writer lock for the duration of a
// command, so that a replica that starts meanwhile waits instead of writing.
// It does nothing when there is no lock file (no entrypoint in use).
func acquireOfflineLock(dataDir string) func() {
	path := filepath.Join(dataDir, singleWriterLockName)
	if _, err := os.Stat(path); err != nil {
		return func() {}
	}

	held, release, err := tryLockFile(path)
	if err != nil || held {
		return func() {}
	}

	return release
}

type keysOpFlags struct {
	collection string
	field      string
	batchSize  int
	offlineOK  bool
	newKEK     bool
	off        bool
}

func (f *keysOpFlags) options(out io.Writer) core.FieldKeyOptions {
	return core.FieldKeyOptions{
		Collection: f.collection,
		Field:      f.field,
		BatchSize:  f.batchSize,
		Progress: func(p core.FieldKeyProgress) {
			switch p.Message {
			case "started":
				fmt.Fprintf(out, "%s.%s: %d row(s) to process\n", p.Collection, p.Field, p.Total)
			case "batch":
				fmt.Fprintf(out, "%s.%s: %d/%d\n", p.Collection, p.Field, p.Done, p.Total)
			}
		},
	}
}

func addTargetFlags(c *cobra.Command, f *keysOpFlags, requireCollection bool) {
	desc := "restrict to this collection (name or id)"
	if requireCollection {
		desc = "the collection (name or id)"
	}
	c.Flags().StringVar(&f.collection, "collection", "", desc)
	c.Flags().StringVar(&f.field, "field", "", "restrict to this field (name or id; needs --collection)")
}

func addRunFlags(c *cobra.Command, f *keysOpFlags) {
	c.Flags().BoolVar(&f.offlineOK, "offline-ok", false,
		"run even though a server may own the data directory (only if you are sure none is running)")
}

// keysRun is the shared RunE: live-server check, offline lock, bootstrap,
// audit context. It is why these commands are exempt from the eager
// bootstrap (see skipBootstrap in pocketbase.go).
func keysRun(app core.App, f *keysOpFlags, command *cobra.Command, fn func(ctx context.Context, out io.Writer) error) error {
	if !f.offlineOK {
		if reason := ServerLiveReason(app.DataDir()); reason != "" {
			return fmt.Errorf("refusing to run: %s.\n"+
				"The server is the single writer of %s, and this command opens the same database.\n"+
				"Run the operation inside the serving process instead (POST /api/field-keys/..., or the dashboard),\n"+
				"or stop the server first. If you are sure no server is running, pass --offline-ok.", reason, app.DataDir())
		}
	}

	release := acquireOfflineLock(app.DataDir())
	defer release()

	if !app.IsBootstrapped() {
		if err := app.Bootstrap(); err != nil {
			return err
		}
	}

	ctx := core.WithAuditContext(command.Context(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")
	if command.Context() == nil {
		ctx = core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")
	}

	return fn(ctx, command.OutOrStdout())
}

func keysStatusCommand(app core.App) *cobra.Command {
	f := &keysOpFlags{}
	c := &cobra.Command{
		Use:          "status",
		Short:        "Shows the field keys and the pending rotation work",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return keysRun(app, f, command, func(ctx context.Context, out io.Writer) error {
				report, err := core.FieldKeysStatus(ctx, app)
				if err != nil {
					return err
				}

				fmt.Fprintf(out, "key-encryption key: %s", report.KekId)
				if report.HasPreviousKek {
					fmt.Fprint(out, " (a previous KEK is configured: rotation in progress)")
				}
				fmt.Fprintln(out)

				if len(report.Fields) == 0 {
					fmt.Fprintln(out, "no encrypted fields")
					return nil
				}

				for _, s := range report.Fields {
					fmt.Fprintf(out, "%s.%s (%s)\n  key %s, wrapped under %s%s, strict=%v\n  rows: %d on the current key, %d on old keys, %d unencrypted\n",
						s.Collection, s.Field, s.Type, s.KeyId, s.KekId, map[bool]string{true: " (needs rewrap)"}[s.NeedsRewrap], s.Strict,
						s.RowsOnCurrentKey, s.RowsOnOldKeys, s.RowsUnencrypted)
					for _, r := range s.Retired {
						fmt.Fprintf(out, "  retired key %s: %d row(s) still use it%s\n", r.KeyId, r.Rows, map[bool]string{true: " (can be pruned)"}[r.Rows == 0])
					}
				}
				return nil
			})
		},
	}
	addRunFlags(c, f)
	return c
}

func keysRotateCommand(app core.App) *cobra.Command {
	f := &keysOpFlags{}
	c := &cobra.Command{
		Use:   "rotate",
		Short: "Rotates the data keys (re-encrypts the rows) or, with --new-kek, re-wraps them",
		Long: "Creates a new data key for every encrypted field (or the selected one), marks the old\n" +
			"one retired and re-encrypts the rows under the new key, 500 rows per transaction.\n" +
			"It can be interrupted and run again: it continues with the same new key and skips\n" +
			"the rows already moved. When no row uses a retired key any more, `keys prune`\n" +
			"deletes it.\n\n" +
			"--new-kek instead re-wraps every data key under the configured key-encryption key\n" +
			"without touching any row (see `keys rewrap`).\n\n" +
			"This command refuses to run while a server owns the data directory unless\n" +
			"--offline-ok; to rotate a live system use the API or the dashboard.",
		Example:      "keys rotate --collection people --field phone\nkeys rotate --new-kek",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if f.newKEK && (f.collection != "" || f.field != "") {
				return errors.New("--new-kek re-wraps every data key and cannot be combined with --collection or --field")
			}
			if f.newKEK {
				return rewrapRun(app, f, command)
			}
			return keysRun(app, f, command, func(ctx context.Context, out io.Writer) error {
				res, err := core.RotateFieldKeys(ctx, app, f.options(out))
				for _, r := range fieldsOrNil(res) {
					fmt.Fprintf(out, "%s.%s: key %s%s, %d re-encrypted, %d skipped (rewritten meanwhile), %d left on old keys\n",
						r.Collection, r.Field, r.NewKeyId, map[bool]string{true: " (resumed)"}[r.Resumed], r.Reencrypted, r.Skipped, r.Failed)
				}
				return err
			})
		},
	}
	addTargetFlags(c, f, false)
	addRunFlags(c, f)
	c.Flags().IntVar(&f.batchSize, "batch-size", core.DefaultFieldKeyBatchSize, "rows per transaction")
	c.Flags().BoolVar(&f.newKEK, "new-kek", false, "re-wrap the data keys under the configured KEK instead (alias of `keys rewrap`)")
	return c
}

func rewrapRun(app core.App, f *keysOpFlags, command *cobra.Command) error {
	return keysRun(app, f, command, func(ctx context.Context, out io.Writer) error {
		res, err := core.RewrapFieldKeys(ctx, app)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%d data key(s) re-wrapped under %s, %d already used it\n", res.Rewrapped, res.KekId, res.Unchanged)
		return nil
	})
}

func keysRewrapCommand(app core.App) *cobra.Command {
	f := &keysOpFlags{}
	c := &cobra.Command{
		Use:   "rewrap",
		Short: "Re-wraps the data keys under the configured key-encryption key (no row data is touched)",
		Long: "KEK rotation. The new KEK is the configured one (CLAVIGER_KEK, CLAVIGER_KEK_FILE or the\n" +
			"app encryption key); the previous one must be available too, as CLAVIGER_KEK_OLD,\n" +
			"CLAVIGER_KEK_FILE_OLD, or CLAVIGER_KEK_OLD_FROM_APP_KEY=1 when the previous KEK is the\n" +
			"one derived from the app encryption key (PB_ENCRYPTION_KEY). If a data key was wrapped\n" +
			"under a KEK that is not available, nothing is changed and the command fails.\n\n" +
			"Afterwards remove the *_OLD variable and restart.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return rewrapRun(app, f, command)
		},
	}
	addRunFlags(c, f)
	return c
}

func keysConvertCommand(app core.App) *cobra.Command {
	f := &keysOpFlags{}
	c := &cobra.Command{
		Use:   "convert",
		Short: "Encrypts the unencrypted legacy values of an encrypted field in place",
		Long: "After a text (or json) field was changed to an encrypted type, the values already in\n" +
			"the column are read as plaintext. This encrypts them, in batches. It is idempotent and\n" +
			"resumable. Afterwards `keys strict` can make unencrypted values a read error.",
		Example:      "keys convert --collection people --field phone",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if f.collection == "" {
				return errors.New("--collection is required")
			}
			return keysRun(app, f, command, func(ctx context.Context, out io.Writer) error {
				res, err := core.ConvertFieldValues(ctx, app, f.options(out))
				for _, r := range convertedOrNil(res) {
					fmt.Fprintf(out, "%s.%s: %d converted, %d skipped (rewritten meanwhile)\n", r.Collection, r.Field, r.Converted, r.Skipped)
				}
				return err
			})
		},
	}
	addTargetFlags(c, f, true)
	addRunFlags(c, f)
	c.Flags().IntVar(&f.batchSize, "batch-size", core.DefaultFieldKeyBatchSize, "rows per transaction")
	return c
}

func keysStrictCommand(app core.App) *cobra.Command {
	f := &keysOpFlags{}
	c := &cobra.Command{
		Use:   "strict",
		Short: "Turns strict mode of an encrypted field on (or off with --off)",
		Long: "In strict mode a stored value without the encryption prefix is a read error instead of\n" +
			"being read as legacy plaintext. It is refused while such values exist: run\n" +
			"`keys convert` first. The flag is stored with the field's key, not in the field options.",
		Example:      "keys strict --collection people --field phone",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if f.collection == "" || f.field == "" {
				return errors.New("--collection and --field are required")
			}
			return keysRun(app, f, command, func(ctx context.Context, out io.Writer) error {
				if err := core.SetFieldKeysStrict(ctx, app, f.collection, f.field, !f.off); err != nil {
					return err
				}
				fmt.Fprintf(out, "%s.%s: strict mode %s\n", f.collection, f.field, map[bool]string{true: "off", false: "on"}[f.off])
				return nil
			})
		},
	}
	addTargetFlags(c, f, true)
	addRunFlags(c, f)
	c.Flags().BoolVar(&f.off, "off", false, "turn strict mode off")
	return c
}

func keysPruneCommand(app core.App) *cobra.Command {
	f := &keysOpFlags{}
	c := &cobra.Command{
		Use:   "prune",
		Short: "Deletes the retired data keys that no row uses any more",
		Long: "A retired key is kept while any row is still encrypted under it. The active key is\n" +
			"never deleted. A database backup taken before the prune still holds the pruned key.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return keysRun(app, f, command, func(ctx context.Context, out io.Writer) error {
				res, err := core.PruneFieldKeys(ctx, app, f.options(out))
				if res != nil {
					fmt.Fprintf(out, "pruned %d key(s)%s\n", len(res.Pruned), pruneList(res.Pruned))
					for id, n := range res.InUse {
						fmt.Fprintf(out, "kept %s: %d row(s) still use it\n", id, n)
					}
				}
				return err
			})
		},
	}
	addTargetFlags(c, f, false)
	addRunFlags(c, f)
	return c
}

func pruneList(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ": " + strings.Join(ids, ", ")
}

func fieldsOrNil(r *core.FieldKeyRotateResult) []core.FieldKeyRotateFieldResult {
	if r == nil {
		return nil
	}
	return r.Fields
}

func convertedOrNil(r *core.FieldKeyConvertResult) []core.FieldKeyConvertFieldResult {
	if r == nil {
		return nil
	}
	return r.Fields
}
