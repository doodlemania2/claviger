// Fork-local (Claviger #110): audit log retention.
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
        ),
    );
}
