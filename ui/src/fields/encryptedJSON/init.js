// Fork-local (Claviger #75): "encryptedJSON" field type.
//
// The editor, the table cell and the save-time JSON check are the ones of the
// regular json field; only the settings differ.
import { input } from "../json/input";
import { onrecordsave } from "../json/onrecordsave";
import { view } from "../json/view";
import { settings } from "./settings";

window.app = window.app || {};
window.app.fieldTypes = window.app.fieldTypes || {};
window.app.fieldTypes.encryptedJSON = {
    icon: "ri-lock-2-line",
    label: "Encrypted JSON",
    settings,
    input,
    view,
    onrecordsave,
    // encrypted values cannot be compared, so the field is never offered
    // in the filter, rule and search autocompletion
    identifierExtractor: (f, prefix = "") => [],
    filterModifiers: (f) => [],
    // the records list table must not offer sorting by it
    sortable: false,
    dummyData: (f, forSubmit = false) => {
        return { "example": "secret" };
    },
};
