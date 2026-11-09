package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/backupenc"
	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/spf13/cobra"
)

// Fork-local (Claviger #78).

// NewBackupsCommand creates the `backups` command group.
func NewBackupsCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "backups",
		Short: "Work with backup archives",
	}

	command.AddCommand(backupsDecryptCommand(app))

	return command
}

func backupsDecryptCommand(app core.App) *cobra.Command {
	var out string
	var force bool

	command := &cobra.Command{
		Use:   "decrypt <file>",
		Short: "Decrypts an encrypted backup (.zip.enc) into a plain zip archive",
		Long: "Decrypts an encrypted backup offline, without starting the app and without touching pb_data.\n\n" +
			"The key-encryption key comes from the same environment as the app: CLAVIGER_KEK,\n" +
			"CLAVIGER_KEK_FILE, CLAVIGER_KEK_AZURE_KEY_ID or the app encryption key. For a backup that was\n" +
			"made with a previous key, also provide it as CLAVIGER_KEK_OLD, CLAVIGER_KEK_FILE_OLD,\n" +
			"CLAVIGER_KEK_AZURE_KEY_ID_OLD or CLAVIGER_KEK_OLD_FROM_APP_KEY.\n\n" +
			"The output defaults to the input name without the .enc suffix. The archive is written to a\n" +
			"temporary file and moved into place only after every chunk was authenticated, so a wrong key,\n" +
			"a truncated or a tampered file leaves nothing behind.",
		Example:      "backups decrypt pb_backup_app_20260101000000.zip.enc --out restored.zip",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			ks, err := keys.LoadWithOld(os.Getenv, app.EncryptionEnv())
			if err != nil {
				return fmt.Errorf("failed to load the key-encryption key: %w", err)
			}

			ctx := command.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			defer cancel()

			dest, err := DecryptBackupFile(ctx, args[0], out, ks, force)
			if err != nil {
				return err
			}

			fmt.Fprintln(command.OutOrStdout(), "decrypted backup written to "+dest)

			return nil
		},
	}

	command.Flags().StringVar(&out, "out", "", "output file (default: the input name without the .enc suffix)")
	command.Flags().BoolVar(&force, "force", false, "overwrite the output file if it exists")

	return command
}

// DecryptBackupFile decrypts the encrypted backup at srcPath into outPath
// (when empty, srcPath without its ".enc" suffix) and returns the output path.
func DecryptBackupFile(ctx context.Context, srcPath, outPath string, ks keys.KeySource, force bool) (string, error) {
	if outPath == "" {
		if !strings.HasSuffix(srcPath, core.BackupEncryptedExt) {
			return "", errors.New("the file name does not end with .enc, set the output file with --out")
		}
		outPath = strings.TrimSuffix(srcPath, core.BackupEncryptedExt)
	}

	if absSrc, err := filepath.Abs(srcPath); err == nil {
		if absOut, err := filepath.Abs(outPath); err == nil && absSrc == absOut {
			return "", errors.New("the output file must differ from the input file")
		}
	}

	if _, err := os.Stat(outPath); err == nil && !force {
		return "", fmt.Errorf("%s already exists (use --force to overwrite it)", outPath)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	hdr, err := backupenc.ReadHeader(src)
	if err != nil {
		return "", fmt.Errorf("%s: %w", srcPath, err)
	}

	if _, err := src.Seek(0, 0); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".decrypt_*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	_, err = backupenc.Decrypt(ctx, tmp, src, ks)
	if err = errors.Join(err, tmp.Close()); err != nil {
		if errors.Is(err, keys.ErrKeyIDMismatch) {
			return "", fmt.Errorf("the backup was encrypted with key %s, which is not available "+
				"(provide it as CLAVIGER_KEK_OLD, CLAVIGER_KEK_FILE_OLD or CLAVIGER_KEK_AZURE_KEY_ID_OLD): %w", hdr.KeyID, err)
		}
		return "", err
	}

	if err := os.Rename(tmp.Name(), outPath); err != nil {
		return "", err
	}

	return outPath, nil
}
