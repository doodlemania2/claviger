package cmd_test

import (
	"archive/zip"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #78). Not parallel: keys come from the environment.

func newKEKB64(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func setBackupKeys(t *testing.T, kek, old string) {
	t.Helper()
	t.Setenv("CLAVIGER_KEK", kek)
	t.Setenv("CLAVIGER_KEK_FILE", "")
	t.Setenv("CLAVIGER_KEK_OLD", old)
	t.Setenv("CLAVIGER_KEK_FILE_OLD", "")
	t.Setenv("CLAVIGER_KEK_OLD_FROM_APP_KEY", "")
}

func runBackups(t testing.TB, app core.App, args ...string) (string, error) {
	t.Helper()
	c := cmd.NewBackupsCommand(app)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func TestBackupsDecryptCommand(t *testing.T) {
	kek := newKEKB64(t)
	setBackupKeys(t, kek, "")

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	app.Settings().Backups.Encrypt = true
	if err := app.CreateBackup(context.Background(), "off.zip"); err != nil {
		t.Fatal(err)
	}

	enc := filepath.Join(app.DataDir(), core.LocalBackupsDirName, "off.zip.enc")
	work := t.TempDir()
	copyTo := filepath.Join(work, "off.zip.enc")
	raw, err := os.ReadFile(enc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyTo, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	assertZip := func(path string) {
		t.Helper()
		zr, err := zip.OpenReader(path)
		if err != nil {
			t.Fatalf("%s is not a zip: %v", path, err)
		}
		defer zr.Close()
		found := false
		for _, f := range zr.File {
			if f.Name == "auxiliary.db" {
				found = true
			}
		}
		if !found {
			t.Fatal("auxiliary.db missing from the decrypted archive")
		}
	}

	t.Run("default output name", func(t *testing.T) {
		out, err := runBackups(t, app, "decrypt", copyTo)
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		assertZip(filepath.Join(work, "off.zip"))
	})

	t.Run("refuses to overwrite", func(t *testing.T) {
		if _, err := runBackups(t, app, "decrypt", copyTo); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected an overwrite error, got %v", err)
		}
		if _, err := runBackups(t, app, "decrypt", copyTo, "--force"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("explicit output", func(t *testing.T) {
		dest := filepath.Join(work, "restored.zip")
		if _, err := runBackups(t, app, "decrypt", copyTo, "--out", dest); err != nil {
			t.Fatal(err)
		}
		assertZip(dest)
	})

	t.Run("wrong key leaves nothing behind", func(t *testing.T) {
		setBackupKeys(t, newKEKB64(t), "")
		dest := filepath.Join(work, "wrongkey.zip")
		_, err := runBackups(t, app, "decrypt", copyTo, "--out", dest)
		if err == nil || !strings.Contains(err.Error(), "CLAVIGER_KEK_OLD") {
			t.Fatalf("expected a clear wrong key error, got %v", err)
		}
		entries, _ := os.ReadDir(work)
		for _, e := range entries {
			if e.Name() == "wrongkey.zip" || strings.HasPrefix(e.Name(), ".decrypt_") {
				t.Fatalf("unexpected leftover %s", e.Name())
			}
		}
	})

	t.Run("rotated key with the old one provided", func(t *testing.T) {
		setBackupKeys(t, newKEKB64(t), kek)
		dest := filepath.Join(work, "rotated.zip")
		if out, err := runBackups(t, app, "decrypt", copyTo, "--out", dest); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		assertZip(dest)
	})

	t.Run("tampered file leaves nothing behind", func(t *testing.T) {
		setBackupKeys(t, kek, "")
		bad := bytes.Clone(raw)
		bad[len(bad)-5] ^= 1
		badPath := filepath.Join(work, "bad.zip.enc")
		if err := os.WriteFile(badPath, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runBackups(t, app, "decrypt", badPath); err == nil {
			t.Fatal("expected an error")
		}
		if _, err := os.Stat(filepath.Join(work, "bad.zip")); err == nil {
			t.Fatal("expected no output file")
		}
	})

	t.Run("not an encrypted backup", func(t *testing.T) {
		plain := filepath.Join(work, "plain.zip.enc")
		if err := os.WriteFile(plain, []byte("PK\x03\x04nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runBackups(t, app, "decrypt", plain); err == nil || !strings.Contains(err.Error(), "not an encrypted") {
			t.Fatalf("expected a not encrypted error, got %v", err)
		}
	})

	t.Run("needs --out without the .enc suffix", func(t *testing.T) {
		noext := filepath.Join(work, "noext")
		if err := os.WriteFile(noext, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runBackups(t, app, "decrypt", noext); err == nil || !strings.Contains(err.Error(), "--out") {
			t.Fatalf("expected a --out hint, got %v", err)
		}
	})
}
