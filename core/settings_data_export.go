package core

import (
	"os"
	"strconv"
	"strings"
	"sync"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
)

// Defaults of the personal data export limits (#88).
const (
	DataExportDefaultMaxFileMB         = 100
	DataExportDefaultMaxTotalMB        = 1024
	DataExportDefaultRetentionDays     = 7
	DataExportDefaultSelfRequestsHour  = 3
	DataExportDefaultAdminRequestsHour = 60
)

// Upper limits an admin can set for the personal data export (#88):
// 1 GB per file, 10 GB in all, 30 days of retention.
const (
	DataExportMaxFileMBLimit     = 1024
	DataExportMaxTotalMBLimit    = 10240
	DataExportRetentionDaysLimit = 30
)

// DataExportConfig defines the limits of the personal data export (#88).
// A field left at 0 means "use the default".
//
// The CLAVIGER_EXPORT_MAX_FILE_MB, CLAVIGER_EXPORT_MAX_TOTAL_MB and
// CLAVIGER_EXPORT_RETENTION_DAYS environment variables are deprecated. When
// one is set to a valid number it still wins over the setting (so an upgrade
// does not change an existing deployment) and a one-time warning is logged.
type DataExportConfig struct {
	// MaxFileMB is the largest single file (of a file field) put in the ZIP.
	MaxFileMB int `form:"maxFileMB" json:"maxFileMB"`

	// MaxTotalMB is the total size of all files put in the ZIP.
	MaxTotalMB int `form:"maxTotalMB" json:"maxTotalMB"`

	// RetentionDays is how long a finished export can be downloaded.
	RetentionDays int `form:"retentionDays" json:"retentionDays"`

	// SelfRequestsPerHour is how many exports a person can request for
	// themselves per hour.
	SelfRequestsPerHour int `form:"selfRequestsPerHour" json:"selfRequestsPerHour"`

	// AdminRequestsPerHour is how many exports a superuser can request per hour.
	AdminRequestsPerHour int `form:"adminRequestsPerHour" json:"adminRequestsPerHour"`
}

func defaultDataExportConfig() DataExportConfig {
	return DataExportConfig{
		MaxFileMB:            DataExportDefaultMaxFileMB,
		MaxTotalMB:           DataExportDefaultMaxTotalMB,
		RetentionDays:        DataExportDefaultRetentionDays,
		SelfRequestsPerHour:  DataExportDefaultSelfRequestsHour,
		AdminRequestsPerHour: DataExportDefaultAdminRequestsHour,
	}
}

// Validate makes DataExportConfig validatable by implementing [validation.Validatable] interface.
func (c DataExportConfig) Validate() error {
	return validation.ValidateStruct(&c,
		validation.Field(&c.MaxFileMB, validation.Min(0), validation.Max(DataExportMaxFileMBLimit)),
		validation.Field(&c.MaxTotalMB, validation.Min(0), validation.Max(DataExportMaxTotalMBLimit)),
		validation.Field(&c.RetentionDays, validation.Min(0), validation.Max(DataExportRetentionDaysLimit)),
		validation.Field(&c.SelfRequestsPerHour, validation.Min(0), validation.Max(1000)),
		validation.Field(&c.AdminRequestsPerHour, validation.Min(0), validation.Max(10000)),
	)
}

var dataExportEnvWarned sync.Map

// dataExportLimitValue returns the effective value: a valid (positive)
// deprecated environment variable first, then the setting, then the default.
func dataExportLimitValue(app App, env string, setting, def int) int {
	if env != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(env))); err == nil && v > 0 {
			if _, seen := dataExportEnvWarned.LoadOrStore(env, true); !seen {
				app.Logger().Warn(
					"Deprecated environment variable for the personal data export limits; set the limit in Settings > Data export instead (the variable still wins while it is set)",
					"env", env,
				)
			}
			return v
		}
	}
	if setting > 0 {
		return setting
	}
	return def
}

// DataExportMaxFileMB returns the effective single file limit in MB.
func DataExportMaxFileMB(app App) int {
	return dataExportLimitValue(app, DataExportMaxFileMBEnv, app.Settings().DataExport.MaxFileMB, DataExportDefaultMaxFileMB)
}

// DataExportMaxTotalMB returns the effective total files limit in MB.
func DataExportMaxTotalMB(app App) int {
	return dataExportLimitValue(app, DataExportMaxTotalMBEnv, app.Settings().DataExport.MaxTotalMB, DataExportDefaultMaxTotalMB)
}

// DataExportRetentionDays returns how many days a finished export stays available.
func DataExportRetentionDays(app App) int {
	return dataExportLimitValue(app, DataExportRetentionDaysEnv, app.Settings().DataExport.RetentionDays, DataExportDefaultRetentionDays)
}

// DataExportSelfRequestsPerHour returns the self service request limit per hour.
func DataExportSelfRequestsPerHour(app App) int {
	return dataExportLimitValue(app, "", app.Settings().DataExport.SelfRequestsPerHour, DataExportDefaultSelfRequestsHour)
}

// DataExportAdminRequestsPerHour returns the superuser request limit per hour.
func DataExportAdminRequestsPerHour(app App) int {
	return dataExportLimitValue(app, "", app.Settings().DataExport.AdminRequestsPerHour, DataExportDefaultAdminRequestsHour)
}
