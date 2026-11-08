// Fork-local (Claviger #74): audit entry detail with the before/after diff.

window.app = window.app || {};
window.app.modals = window.app.modals || {};

window.app.modals.openAuditEntryPreview = function(entryIdOrModel, settings = {}) {
    const modal = auditEntryPreviewModal(entryIdOrModel, settings);
    document.body.appendChild(modal);
    app.modals.open(modal);
};

function show(value) {
    if (value === undefined) {
        return "";
    }
    if (typeof value == "string") {
        return value;
    }
    return JSON.stringify(value);
}

function auditEntryPreviewModal(entryIdOrModel, settings) {
    const data = store({
        isLoading: false,
        entry: null,
    });

    async function load() {
        data.isLoading = true;

        try {
            data.entry = app.utils.isObject(entryIdOrModel)
                ? JSON.parse(JSON.stringify(entryIdOrModel))
                : await app.pb.collection("_auditLog").getOne(entryIdOrModel, { requestKey: "audit_entry_preview" });
            data.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                data.isLoading = false;
                app.checkApiError(err);
            }
        }
    }

    function metaRow(name, value) {
        return t.tr(
            null,
            t.th({ className: "p-r-0" }, name),
            t.td(null, show(value) || t.span({ className: "txt-hint" }, "N/A")),
            t.td({ className: "min-width" }, app.components.copyButton(show(value))),
        );
    }

    return t.div(
        {
            pbEvent: "auditEntryPreviewModal",
            className: "modal audit-entry-preview-modal",
            onbeforeopen: (el) => {
                load();
                return settings.onbeforeopen?.(el);
            },
            onafteropen: (el) => settings.onafteropen?.(el),
            onbeforeclose: (el) => settings.onbeforeclose?.(el),
            onafterclose: (el) => {
                settings.onafterclose?.(el);
                el?.remove();
            },
        },
        t.header(
            { className: "modal-header" },
            t.h5(null, "Audit entry"),
        ),
        t.div({ className: "modal-content" }, () => {
            const e = data.entry;
            if (!e || data.isLoading) {
                return t.div({ className: "block txt-center" }, t.span({ className: "loader" }));
            }

            const names = e.changedFields || [];

            return [
                t.table(
                    { className: "log-view-table responsive-table" },
                    t.tbody(
                        null,
                        metaRow("id", e.id),
                        metaRow("created", e.created),
                        metaRow("action", e.action),
                        metaRow(
                            "actor",
                            e.actorType == "system" || !e.actorId ? "system" : `${e.actorType}: ${e.actorCollection}/${e.actorId}`,
                        ),
                        metaRow("collection", e.targetCollection),
                        metaRow("record", e.targetId),
                        metaRow("ip", e.ip),
                        app.utils.isEmpty(e.meta) ? null : metaRow("meta", e.meta),
                    ),
                ),
                t.h6({ className: "m-t-20" }, "Changes"),
                names.length
                    ? t.table(
                        { className: "audit-diff-table responsive-table" },
                        t.thead(
                            null,
                            t.tr(null, t.th(null, "Field"), t.th(null, "Before"), t.th(null, "After")),
                        ),
                        t.tbody(
                            null,
                            names.map((name) =>
                                t.tr(
                                    null,
                                    t.th({ className: "p-r-0" }, name),
                                    t.td({ className: "txt-danger" }, show(e.before?.[name])),
                                    t.td({ className: "txt-success" }, show(e.after?.[name])),
                                )
                            ),
                        ),
                    )
                    : t.div({ className: "txt-hint" }, "No field changes recorded."),
                t.p(
                    { className: "txt-hint txt-sm m-t-10" },
                    "Passwords, tokens, secrets and hidden fields are never stored; they appear as [redacted].",
                ),
            ];
        }),
        t.footer(
            { className: "modal-footer" },
            t.button(
                { type: "button", className: "btn transparent", onclick: () => app.modals.close() },
                t.span({ className: "txt" }, "Close"),
            ),
        ),
    );
}
