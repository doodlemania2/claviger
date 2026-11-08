// Fork-local (Claviger #75): "encryptedText" field type.
import { input } from "./input";
import { settings } from "./settings";
import { view } from "./view";

window.app = window.app || {};
window.app.fieldTypes = window.app.fieldTypes || {};
window.app.fieldTypes.encryptedText = {
    icon: "ri-lock-2-line",
    label: "Encrypted text",
    settings,
    input,
    view,
    // encrypted values cannot be compared, so the field is never offered
    // in the filter, rule and search autocompletion
    identifierExtractor: (f, prefix = "") => [],
    filterModifiers: (f) => [],
    // the records list table must not offer sorting by it
    sortable: false,
    dummyData: (f, forSubmit = false) => {
        return "example secret text";
    },
};
