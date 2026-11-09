// Fork-local (Claviger #75): encryption keys of the encrypted fields.
//
// Rotation runs inside the server as a background job (POST /api/field-keys/...),
// so it is safe on a live system; this page starts it and shows its progress.

import { settingsSidebar } from "../settingsSidebar";

export function pageFieldKeysSettings() {
    app.store.title = "Encryption keys";

    const data = store({
        status: null,
        job: null,
        isLoading: false,
    });

    let pollTimeoutId;

    function stopPolling() {
        clearTimeout(pollTimeoutId);
        pollTimeoutId = null;
    }

    async function pollJob() {
        stopPolling();

        try {
            const res = await app.pb.send("/api/field-keys/job", { requestKey: "fieldkeys_job" });
            data.job = res.job;

            if (res.job?.state === "running") {
                pollTimeoutId = setTimeout(pollJob, 1000);
                return;
            }

            // finished: refresh the numbers
            load();
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    async function load() {
        data.isLoading = true;

        try {
            const res = await app.pb.send("/api/field-keys", { requestKey: "fieldkeys_status" });
            data.status = res.status;
            data.job = res.job;
            data.isLoading = false;

            if (res.job?.state === "running" && !pollTimeoutId) {
                pollTimeoutId = setTimeout(pollJob, 1000);
            }
        } catch (err) {
            if (!err.isAbort) {
                data.isLoading = false;
                app.checkApiError(err);
            }
        }
    }

    async function start(op, body, message) {
        try {
            const res = await app.pb.send("/api/field-keys/" + op, { method: "POST", body: body || {} });
            data.job = res.job;
            app.toasts.success(message);
            pollTimeoutId = setTimeout(pollJob, 500);
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function confirmStart(op, body, text, message) {
        return app.modals.confirm(text, () => start(op, body, message));
    }

    async function setStrict(field, strict) {
        try {
            await app.pb.send("/api/field-keys/strict", {
                method: "POST",
                body: { collection: field.collection, field: field.field, strict },
            });
            app.toasts.success(`Strict mode ${strict ? "enabled" : "disabled"} for ${field.collection}.${field.field}.`);
            load();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function isRunning() {
        return data.job?.state === "running";
    }

    function jobBox() {
        const job = data.job;
        if (!job) {
            return null;
        }

        const p = job.progress || {};
        let text = "";
        let cls = "info";

        if (job.state === "running") {
            text = `Running: ${job.op}` +
                (p.collection ? ` ${p.collection}.${p.field}: ${p.done || 0}/${p.total || 0} rows` : "") + "...";
        } else if (job.state === "failed") {
            cls = "danger";
            text = `${job.op} failed: ${job.error}`;
        } else {
            cls = "success";
            text = `${job.op} finished.`;
        }

        return t.div({ className: `alert ${cls} m-b-sm` }, t.div({ className: "content" }, text));
    }

    function actions(f) {
        return t.div(
            { className: "inline-flex gap-sm" },
            t.button(
                {
                    type: "button",
                    className: "btn sm secondary",
                    disabled: () => isRunning(),
                    onclick: () =>
                        confirmStart(
                            "rotate",
                            { collection: f.collection, field: f.field },
                            `Rotate the data key of ${f.collection}.${f.field}? A new key is created and every row is re-encrypted in the background. Reads and writes keep working meanwhile.`,
                            "Rotation started.",
                        ),
                },
                t.span({ className: "txt" }, "Rotate"),
            ),
            f.rowsUnencrypted > 0
                ? t.button(
                    {
                        type: "button",
                        className: "btn sm secondary",
                        disabled: () => isRunning(),
                        onclick: () =>
                            confirmStart(
                                "convert",
                                { collection: f.collection, field: f.field },
                                `Encrypt the ${f.rowsUnencrypted} unencrypted value(s) of ${f.collection}.${f.field}?`,
                                "Conversion started.",
                            ),
                    },
                    t.span({ className: "txt" }, "Encrypt legacy values"),
                )
                : null,
            t.button(
                {
                    type: "button",
                    className: "btn sm secondary transparent",
                    disabled: () => isRunning() || (!f.strict && f.rowsUnencrypted > 0),
                    title: f.rowsUnencrypted > 0 && !f.strict ? "Encrypt the legacy values first" : "",
                    onclick: () => setStrict(f, !f.strict),
                },
                t.span({ className: "txt" }, f.strict ? "Strict: on" : "Strict: off"),
            ),
        );
    }

    return t.div(
        {
            pbEvent: "pageFieldKeysSettings",
            className: "page page-field-keys",
            onmount() {
                load();
            },
            onunmount() {
                stopPolling();
            },
        },
        settingsSidebar(),
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav(
                    { className: "breadcrumbs" },
                    t.div({ className: "breadcrumb-item" }, "Settings"),
                    t.div({ className: "breadcrumb-item" }, () => app.store.title),
                ),
                app.components.refreshButton({ onclick: () => load() }),
                t.div(
                    { className: "inline-flex gap-sm m-l-auto" },
                    t.button(
                        {
                            type: "button",
                            className: "btn secondary",
                            disabled: () => isRunning(),
                            onclick: () =>
                                confirmStart(
                                    "prune",
                                    {},
                                    "Delete the retired data keys that no row uses any more?",
                                    "Prune started.",
                                ),
                        },
                        t.span({ className: "txt" }, "Prune old keys"),
                    ),
                    t.button(
                        {
                            type: "button",
                            className: "btn secondary",
                            hidden: () => !data.status?.fields?.some((f) => f.needsRewrap),
                            disabled: () => isRunning(),
                            onclick: () =>
                                confirmStart(
                                    "rewrap",
                                    {},
                                    "Re-wrap the data keys under the configured key-encryption key? No row is changed. Afterwards remove CLAVIGER_KEK_OLD and restart.",
                                    "Re-wrap started.",
                                ),
                        },
                        t.span({ className: "txt" }, "Re-wrap under the new KEK"),
                    ),
                    t.button(
                        {
                            type: "button",
                            className: "btn",
                            disabled: () => isRunning(),
                            onclick: () =>
                                confirmStart(
                                    "rotate",
                                    {},
                                    "Rotate the data key of EVERY encrypted field? Rows are re-encrypted in the background.",
                                    "Rotation started.",
                                ),
                        },
                        t.i({ className: "ri-refresh-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Rotate all"),
                    ),
                ),
            ),
            t.div(
                { className: "wrapper m-b-base" },
                t.div(
                    { className: "txt-hint m-b-sm" },
                    "Each encrypted field has its own data key, wrapped by the key-encryption key (",
                    t.code(null, "CLAVIGER_KEK"),
                    "). Rotating a key re-encrypts the field's rows in the background; the old key is retired and can be pruned once no row uses it.",
                ),
                () => jobBox(),
                () => {
                    if (data.status?.hasPreviousKek) {
                        return t.div(
                            { className: "alert warning m-b-sm" },
                            t.div(
                                { className: "content" },
                                "A previous key-encryption key is configured (CLAVIGER_KEK_OLD): a KEK rotation is in progress.",
                            ),
                        );
                    }
                },
                t.table(
                    { className: "field-keys-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Field"),
                            t.th(null, "Key"),
                            t.th(null, "Rows (current / old / unencrypted)"),
                            t.th(null, "Retired keys"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(
                        null,
                        () => {
                            const fields = data.status?.fields || [];

                            if (!fields.length) {
                                return t.tr(
                                    null,
                                    t.td(
                                        { colSpan: 99 },
                                        data.isLoading
                                            ? t.span({ className: "skeleton-loader" })
                                            : t.div({ className: "txt-center txt-hint" }, "No encrypted fields."),
                                    ),
                                );
                            }

                            return fields.map((f) =>
                                t.tr(
                                    // shablon reuses a row with the same rid as is: every shown value must be part of it,
                                    // or a rotation or prune leaves the old key and counts on screen
                                    {
                                        rid: [
                                            f.collection + "." + f.field,
                                            f.keyId,
                                            f.needsRewrap,
                                            f.rowsOnCurrentKey,
                                            f.rowsOnOldKeys,
                                            f.rowsUnencrypted,
                                            (f.retired || []).map((r) => r.keyId + ":" + r.rows).join(","),
                                        ].join("|"),
                                    },
                                    t.td(null, `${f.collection}.${f.field}`),
                                    t.td(null, t.code(null, f.keyId), f.needsRewrap ? " (old KEK)" : ""),
                                    t.td(null, `${f.rowsOnCurrentKey} / ${f.rowsOnOldKeys} / ${f.rowsUnencrypted}`),
                                    t.td(
                                        null,
                                        (f.retired || []).length
                                            ? (f.retired || []).map((r) => `${r.keyId} (${r.rows} rows)`).join(", ")
                                            : "-",
                                    ),
                                    t.td({ className: "col-meta" }, actions(f)),
                                )
                            );
                        },
                    ),
                ),
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
