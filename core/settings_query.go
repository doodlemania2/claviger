package core

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

type Param struct {
	BaseModel

	Created types.DateTime `db:"created" json:"created"`
	Updated types.DateTime `db:"updated" json:"updated"`
	Value   types.JSONRaw  `db:"value" json:"value"`
}

func (m *Param) TableName() string {
	return paramsTable
}

// ReloadSettings initializes and reloads the stored application settings.
//
// If no settings were stored it will persist the current app ones.
func (app *BaseApp) ReloadSettings() error {
	param := &Param{}
	err := app.ModelQuery(param).Model(paramsKeySettings, param)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// no settings were previously stored -> save
	// (ReloadSettings() will be invoked again by a system hook after successful save)
	if param.Id == "" {
		// force insert in case the param entry was deleted manually after application start
		app.Settings().MarkAsNew()
		saveErr := app.Save(app.Settings())

		// several replicas (#106 M7): on the very first start of an empty
		// database two of them insert the defaults at the same time and one
		// loses on the primary key. The winner's row is the settings.
		if saveErr == nil || !IsMultiReplica(app) {
			return saveErr
		}
		if err := app.ModelQuery(param).Model(paramsKeySettings, param); err != nil || param.Id == "" {
			return saveErr
		}
		app.Settings().MarkAsNotNew()
	}

	event := new(SettingsReloadEvent)
	event.App = app

	return app.OnSettingsReload().Trigger(event, func(e *SettingsReloadEvent) error {
		return e.App.Settings().loadParam(e.App, param)
	})
}

// loadParam loads the settings from the stored param into the app ones.
//
// @todo note that the encryption may get removed in the future since it doesn't
// really accomplish much and it might be better to find a way to encrypt the backups
// or implement support for resolving env variables.
func (s *Settings) loadParam(app App, param *Param) error {
	// try first without decryption
	s.mu.Lock()
	plainDecodeErr := json.Unmarshal(param.Value, s)
	s.mu.Unlock()

	// failed, try to decrypt
	if plainDecodeErr != nil {
		encryptionKey := os.Getenv(app.EncryptionEnv())

		// load without decryption has failed and there is no encryption key to use for decrypt
		if encryptionKey == "" {
			return fmt.Errorf("invalid settings db data or missing encryption key %q", app.EncryptionEnv())
		}

		// decrypt
		decrypted, decryptErr := security.Decrypt(string(param.Value), encryptionKey)
		if decryptErr != nil {
			return decryptErr
		}

		// decode again
		s.mu.Lock()
		decryptedDecodeErr := json.Unmarshal(decrypted, s)
		s.mu.Unlock()
		if decryptedDecodeErr != nil {
			return decryptedDecodeErr
		}
	}

	return s.PostScan()
}
