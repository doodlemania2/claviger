// Fork-local (Claviger #77): webhook endpoints and their recent deliveries.

const ENDPOINTS = "_webhooks";
const DELIVERIES = "_webhookDeliveries";
const statusClass = { success: "success", pending: "warning", dead: "danger" };

export function pageWebhooks(route) {
    app.store.title = "Webhooks";

    const state = store({
        endpoints: [],
        deliveries: [],
        activeId: route.query.endpoint?.[0] || "",
        isLoading: false,
        isLoadingDeliveries: false,
        testing: {},
    });

    async function loadEndpoints() {
        state.isLoading = true;
        try {
            state.endpoints = await app.pb.collection(ENDPOINTS).getFullList({ sort: "-created", requestKey: "wh_list" });
            if (!state.activeId && state.endpoints.length) {
                state.activeId = state.endpoints[0].id;
            }
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
        state.isLoading = false;
    }

    async function loadDeliveries() {
        if (!state.activeId) {
            state.deliveries = [];
            return;
        }
        state.isLoadingDeliveries = true;
        try {
            const res = await app.pb.collection(DELIVERIES).getList(1, 50, {
                filter: app.pb.filter("webhook = {:id}", { id: state.activeId }),
                sort: "-created,-id",
                skipTotal: 1,
                requestKey: "wh_deliveries",
            });
            state.deliveries = res.items;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
        state.isLoadingDeliveries = false;
    }

    async function sendTest(ep) {
        state.testing[ep.id] = true;
        try {
            const d = await app.pb.send(`/api/webhooks/${ep.id}/test`, { method: "POST" });
            if (d.status == "success") {
                app.toasts.success("Test event delivered.");
            } else {
                app.toasts.error("Test event failed: " + (d.lastError || "see the delivery log"));
            }
            state.activeId = ep.id;
            await loadDeliveries();
        } catch (err) {
            app.checkApiError(err);
        }
        delete state.testing[ep.id];
    }

    async function toggle(ep) {
        try {
            await app.pb.collection(ENDPOINTS).update(ep.id, { enabled: !ep.enabled });
            await loadEndpoints();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function rotate(ep) {
        app.modals.confirm(
            "Generate a new signing secret? The current one keeps working for 24 hours.",
            async () => {
                try {
                    const res = await app.pb.send(`/api/webhooks/${ep.id}/rotate-secret`, { method: "POST" });
                    app.modals.openWebhookSecret(res.secret);
                } catch (err) {
                    app.checkApiError(err);
                }
            },
        );
    }

    function remove(ep) {
        app.modals.confirm(`Delete the webhook for ${ep.url}? Its delivery log is deleted too.`, async () => {
            try {
                await app.pb.collection(ENDPOINTS).delete(ep.id);
                if (state.activeId == ep.id) {
                    state.activeId = "";
                }
                await loadEndpoints();
            } catch (err) {
                app.checkApiError(err);
            }
        });
    }

    async function redeliver(d) {
        try {
            await app.pb.send(`/api/webhook-deliveries/${d.id}/redeliver`, { method: "POST" });
            app.toasts.success("Delivery re-queued.");
            await loadDeliveries();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function openForm(ep) {
        app.modals.openWebhookModal(ep, { onsave: () => loadEndpoints() });
    }

    function btn(icon, title, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    const watchers = [];

    return t.div(
        {
            pbEvent: "pageWebhooks",
            className: "page page-webhooks",
            onmount() {
                loadEndpoints();
                watchers.push(
                    watch(() => state.activeId, () => {
                        app.utils.replaceHashQueryParams({ endpoint: state.activeId });
                        loadDeliveries();
                    }),
                );
            },
            onunmount() {
                watchers.forEach((w) => w?.unwatch());
            },
        },
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav({ className: "breadcrumbs" }, t.div(null, "Webhooks")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({
                        onclick: () => {
                            loadEndpoints();
                            loadDeliveries();
                        },
                    }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", onclick: () => openForm(null) },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New webhook"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                t.table(
                    { className: "webhooks-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "URL"),
                            t.th(null, "Events"),
                            t.th(null, "State"),
                            t.th({ className: "col-meta" }, "Actions"),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.endpoints.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    state.isLoading
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div(
                                            { className: "txt-center txt-hint" },
                                            "No webhooks configured. Nothing is sent and no background work runs.",
                                        ),
                                ),
                            );
                        }

                        return state.endpoints.map((ep) =>
                            t.tr(
                                {
                                    className: () => `handle ${state.activeId == ep.id ? "active" : ""}`,
                                    onclick: () => state.activeId = ep.id,
                                },
                                t.td(
                                    null,
                                    t.div({ className: "txt-bold" }, ep.url),
                                    ep.description ? t.div({ className: "txt-hint txt-sm" }, ep.description) : null,
                                ),
                                t.td(null, app.utils.truncate((ep.events || []).join(", "), 80)),
                                t.td(
                                    null,
                                    t.span(
                                        { className: `label sm ${ep.enabled ? "success" : ""}` },
                                        ep.enabled ? "enabled" : "disabled",
                                    ),
                                    ep.disabledReason
                                        ? t.div({ className: "txt-danger txt-sm" }, ep.disabledReason)
                                        : (ep.consecutiveFailures > 0
                                            ? t.div({ className: "txt-warning txt-sm" }, `${ep.consecutiveFailures} consecutive failures`)
                                            : null),
                                ),
                                t.td(
                                    { className: "col-meta", onclick: (e) => e.stopPropagation() },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        btn("ri-send-plane-line", "Send test event", () => sendTest(ep), state.testing[ep.id] ? "loading" : ""),
                                        btn("ri-pencil-line", "Edit", () => openForm(ep)),
                                        btn(ep.enabled ? "ri-pause-line" : "ri-play-line", ep.enabled ? "Disable" : "Enable", () => toggle(ep)),
                                        btn("ri-key-2-line", "Rotate secret", () => rotate(ep)),
                                        btn("ri-delete-bin-7-line", "Delete", () => remove(ep), "danger"),
                                    ),
                                ),
                            )
                        );
                    }),
                ),
                t.h6({ className: "m-t-20 m-b-10" }, "Recent deliveries"),
                t.table(
                    { className: "webhook-deliveries-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "When"),
                            t.th(null, "Event"),
                            t.th(null, "Status"),
                            t.th(null, "Attempts"),
                            t.th(null, "Last result"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.deliveries.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    state.isLoadingDeliveries
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div({ className: "txt-center txt-hint" }, "No deliveries yet."),
                                ),
                            );
                        }

                        return state.deliveries.map((d) =>
                            t.tr(
                                null,
                                t.td(null, app.components.formattedDate({ value: () => d.created, short: false })),
                                t.td(null, d.eventType),
                                t.td(null, t.span({ className: `label sm ${statusClass[d.status] || ""}` }, d.status)),
                                t.td(null, d.attempts),
                                t.td(
                                    null,
                                    d.lastStatusCode ? `HTTP ${d.lastStatusCode} ` : "",
                                    d.lastError ? t.span({ className: "txt-hint" }, app.utils.truncate(d.lastError, 80)) : "",
                                    d.status == "pending" && d.nextAttempt
                                        ? t.span({ className: "txt-hint" }, " next: ", app.components.formattedDate({ value: () => d.nextAttempt, short: false }))
                                        : null,
                                ),
                                t.td(
                                    { className: "col-meta" },
                                    d.eventType != "webhook.test"
                                        ? btn("ri-refresh-line", "Redeliver", () => redeliver(d))
                                        : null,
                                ),
                            )
                        );
                    }),
                ),
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
