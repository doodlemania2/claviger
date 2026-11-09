// Fork-local (Claviger #95): API keys management page.

import { settingsSidebar } from "../settingsSidebar";
import { openApiKeyCreateModal } from "./apiKeyCreateModal";

export function pageApiKeysSettings() {
    app.store.title = "API keys";

    const data = store({
        keys: [],
        showAll: false,
        isLoading: false,
    });

    async function load() {
        data.isLoading = true;

        try {
            const result = await app.pb.send("/api/apikeys", {
                query: { all: data.showAll ? "true" : "false" },
                requestKey: "apikeys_list",
            });
            data.keys = result.items || [];
            data.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                data.isLoading = false;
                app.checkApiError(err);
            }
        }
    }

    function revoke(key) {
        return app.modals.confirm(
            `Do you really want to revoke the API key "${key.name}" (${key.prefix})? Clients using it will stop working immediately.`,
            async () => {
                try {
                    await app.pb.send("/api/apikeys/" + encodeURIComponent(key.id), { method: "DELETE" });
                    app.toasts.success("The API key was revoked.");
                    load();
                } catch (err) {
                    app.checkApiError(err);
                }
            },
        );
    }

    function status(key) {
        if (key.revoked) {
            return t.span({ className: "label danger" }, "Revoked");
        }
        if (key.expires && new Date(key.expires.replace(" ", "T")) < new Date()) {
            return t.span({ className: "label warning" }, "Expired");
        }
        return t.span({ className: "label success" }, "Active");
    }

    let watcher;

    return t.div(
        {
            pbEvent: "pageApiKeysSettings",
            className: "page page-api-keys",
            onmount() {
                watcher = watch(() => data.showAll, () => load());
                load();
            },
            onunmount() {
                watcher?.unwatch();
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
                    t.div(
                        { className: "field" },
                        t.input({
                            type: "checkbox",
                            id: "apikeys_show_all",
                            className: "switch",
                            checked: () => data.showAll,
                            onchange: (e) => data.showAll = e.target.checked,
                        }),
                        t.label({ htmlFor: "apikeys_show_all" }, "Show keys of all accounts"),
                    ),
                    t.button(
                        {
                            type: "button",
                            className: "btn expanded-lg",
                            onclick: () => openApiKeyCreateModal({ oncreated: () => load() }),
                        },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New API key"),
                    ),
                ),
            ),
            t.div(
                { className: "wrapper m-b-base" },
                t.div(
                    { className: "txt-hint m-b-sm" },
                    "API keys authenticate a request as their owner, limited by their scopes. Use them with ",
                    t.code(null, "Authorization: Bearer clv_..."),
                    ". The full key is shown only once, when created.",
                ),
                t.table(
                    { className: "api-keys-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Name"),
                            t.th(null, "Prefix"),
                            t.th(null, "Scopes"),
                            t.th(null, "Last used"),
                            t.th(null, "Expires"),
                            t.th(null, "Status"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(
                        null,
                        () => {
                            if (!data.keys.length) {
                                return t.tr(
                                    null,
                                    t.td(
                                        { colSpan: 99 },
                                        data.isLoading
                                            ? t.span({ className: "skeleton-loader" })
                                            : t.div({ className: "txt-center txt-hint" }, "No API keys."),
                                    ),
                                );
                            }

                            return data.keys.map((key) =>
                                t.tr(
                                    // shablon reuses a row with the same rid as is: the state must be part of it,
                                    // or a revoked key keeps showing as Active until the page is reloaded
                                    { rid: key.id + ":" + (key.revoked ? "revoked" : "active") + ":" + (key.lastUsed || "") },
                                    t.td(null, key.name),
                                    t.td(null, t.code(null, "clv_" + key.prefix + "_...")),
                                    t.td({ className: "api-key-scopes" }, (key.scopes || []).join(", ")),
                                    t.td(null, key.lastUsed ? app.utils.toLocalDatetime(key.lastUsed) : "Never"),
                                    t.td(null, key.expires ? app.utils.toLocalDatetime(key.expires) : "Never"),
                                    t.td(null, status(key)),
                                    t.td(
                                        { className: "col-meta" },
                                        key.revoked ? null : t.button(
                                            {
                                                type: "button",
                                                className: "btn sm danger transparent",
                                                onclick: () => revoke(key),
                                            },
                                            t.span({ className: "txt" }, "Revoke"),
                                        ),
                                    ),
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
