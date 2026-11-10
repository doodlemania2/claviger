package core

// BuiltinDataExportForTest exposes the export allowlist to the external tests.
var BuiltinDataExportForTest = builtinDataExport

// DataExportFieldsForTest returns the fields the export would write for a
// built-in entry of the collection.
func DataExportFieldsForTest(col *Collection) []string {
	fields, _ := dataExportFieldsFor(col, dataMapRow{DataMapEntry: DataMapEntry{Collection: col.Name}, Builtin: true})
	return fields
}
