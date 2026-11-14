// Fork-local (Claviger #110, #86): audit log retention and read audit.
export function auditAccordion(pageData) {
    return t.details(
        {
            pbEvent: "auditAccordion",
            className: "accordion audit-accordion",
            name: "settingsAccordion",
        },
        t.summary(
            null,
            t.i({ className: "ri-file-list-3-line", ariaHidden: true }),
            t.span({ className: "txt" }, "Audit log"),
            t.div({ className: "flex-fill" }),
            () => {
                const n = pageData.formSettings.audit?.readCollections?.length || 0;
                if (n > 0) {
                    return t.span({ className: "label warning" }, "Reads audited: " + n);
                }
            },
            () => {
                const days = pageData.formSettings.audit?.retentionDays || 0;
                if (days > 0) {
                    return t.span({ className: "label success" }, "Keep " + days + " days");
                }
                return t.span({ className: "label" }, "Keep forever");
            },
            () => {
                if (!app.utils.isEmpty(app.store.errors?.audit)) {
                    return t.i({
                        className: "ri-error-warning-fill txt-danger",
                        ariaDescription: app.attrs.tooltip("Has errors", "left"),
                    });
                }
            },
        ),
        t.div(
            { className: "grid sm" },
            t.div(
                { className: "col-lg-4" },
                t.div(
                    { className: "field" },
                    t.label(
                        { htmlFor: "audit.retentionDays" },
                        t.span({ className: "txt" }, "Retention (in days)"),
                        t.i({
                            className: "ri-information-line link-faded",
                            ariaDescription: app.attrs.tooltip(
                                "Entries older than this are removed by a daily cleanup. Leave empty or 0 to keep them forever. " +
                                    "The CLAVIGER_AUDIT_RETENTION_DAYS environment variable, when set, overrides this value.",
                                "right",
                            ),
                        }),
                    ),
                    t.input({
                        id: "audit.retentionDays",
                        name: "audit.retentionDays",
                        type: "number",
                        min: 0,
                        step: 1,
                        placeholder: "0 (forever)",
                        value: () => pageData.formSettings.audit?.retentionDays || "",
                        oninput: (e) => {
                            pageData.formSettings.audit = pageData.formSettings.audit || {};
                            pageData.formSettings.audit.retentionDays = parseInt(e.target.value, 10) || 0;
                        },
                    }),
                ),
            ),
            t.div(
                { className: "col-lg-8" },
                t.div(
                    { className: "fields" },
                    t.div(
                        { className: "field" },
                        t.label(
                            { htmlFor: "audit.readCollections" },
                            t.span({ className: "txt" }, "Audit reads of collections"),
                            t.i({
                                className: "ri-information-line link-faded",
                                ariaDescription: app.attrs.tooltip(
                                    "Record views, lists, expands and realtime events of these collections are written to the audit log " +
                                        "as \"read\" entries (who, which record ids, when, from where), superusers included. Writes are always audited.",
                                    "right",
                                ),
                            }),
                        ),
                        t.input({
                            type: "text",
                            id: "audit.readCollections",
                            name: "audit.readCollections",
                            placeholder: "Comma separated collection names, e.g. pastoral_notes. Leave empty to audit writes only",
                            value: () => app.utils.joinNonEmpty(pageData.formSettings.audit?.readCollections),
                            oninput: (e) => {
                                const newValue = app.utils.splitNonEmpty(e.target.value, ",");
                                const oldValue = pageData.formSettings.audit?.readCollections;

                                if (app.utils.joinNonEmpty(newValue) != app.utils.joinNonEmpty(oldValue)) {
                                    pageData.formSettings.audit = pageData.formSettings.audit || {};
                                    pageData.formSettings.audit.readCollections = newValue;
                                }
                            },
                        }),
                    ),
                    t.div(
                        { className: "field addon" },
                        t.button(
                            {
                                type: "button",
                                className: () =>
                                    `btn sm secondary transparent ${
                                        app.utils.isEmpty(pageData.formSettings.audit?.readCollections) ? "hidden" : ""
                                    }`,
                                onclick: () => {
                                    pageData.formSettings.audit.readCollections = [];
                                },
                            },
                            t.span({ className: "txt" }, "Clear"),
                        ),
                    ),
                ),
                t.div(
                    { className: "field-help" },
                    "Existing collection names only (system collections such as _orgs are allowed, the audit log itself is not).",
                ),
            ),
        ),
    );
}
