//go:build !no_default_driver

package core

func init() {
	backupVerifyDBConnect = DefaultDBConnect
}
