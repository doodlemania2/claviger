// Fork-local (Claviger #71): OpenID Connect provider. OAuth clients and signing keys.

const CLIENTS = "_oauthClients";
const keyStatusClass = { active: "success", retiring: "warning", retired: "" };

export function pageOidc() {
    app.store.title = "OpenID Connect";

    const state = store({
        status: null,
        clients: [],
        keys: [],
        isLoading: false,
        isRotating: false,
    });

    async function loadStatus() {
        try {
            state.status = await app.pb.send("/api/oidc/status", { requestKey: "oidc_status" });
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    async function loadClients() {
        state.isLoading = true;
        try {
            state.clients = await app.pb.collection(CLIENTS).getFullList({ sort: "-created", requestKey: "oidc_clients" });
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
        state.isLoading = false;
    }

    async function loadKeys() {
        try {
            state.keys = await app.pb.send("/api/oidc/keys", { requestKey: "oidc_keys" });
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    function loadAll() {
        loadStatus();
        loadClients();
        loadKeys();
    }

    function openForm(client) {
        app.modals.openOidcClientModal(client, { onsave: () => loadAll() });
    }

    function rotateSecret(client) {
        app.modals.confirm(
            `Generate a new secret for ${client.name}? The current secret stops working at once.`,
            async () => {
                try {
                    const res = await app.pb.send(`/api/oidc/clients/${client.id}/secret`, { method: "POST" });
                    app.modals.openOidcClientSecret(res.clientId, res.clientSecret);
                } catch (err) {
                    app.checkApiError(err);
                }
            },
        );
    }

    async function toggle(client) {
        try {
            await app.pb.collection(CLIENTS).update(client.id, { disabled: !client.disabled });
            await loadClients();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function remove(client) {
        app.modals.confirm(
            `Delete ${client.name}? Its users can no longer sign in to it; tokens it holds stop working.`,
            async () => {
                try {
                    await app.pb.collection(CLIENTS).delete(client.id);
                    await loadClients();
                } catch (err) {
                    app.checkApiError(err);
                }
            },
        );
    }

    function rotateKeys() {
        app.modals.confirm(
            "Create new signing keys? The current keys stay published for 7 days so tokens already issued keep verifying.",
            async () => {
                state.isRotating = true;
                try {
                    await app.pb.send("/api/oidc/keys/rotate", { method: "POST", body: {} });
                    app.toasts.success("New signing keys created.");
                    await loadKeys();
                } catch (err) {
                    app.checkApiError(err);
                }
                state.isRotating = false;
            },
        );
    }

    function btn(icon, title, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    return t.div(
        {
            pbEvent: "pageOidc",
            className: "page page-oidc",
            onmount() {
                loadAll();
            },
        },
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav({ className: "breadcrumbs" }, t.div(null, "OpenID Connect")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({ onclick: loadAll }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", onclick: () => openForm(null) },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New client"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                () => {
                    const s = state.status;
                    if (!s) {
                        return null;
                    }
                    if (!s.enabled) {
                        return t.div(
                            { className: "alert warning m-b-10" },
                            "The OpenID Connect provider is off: " + s.reason,
                        );
                    }
                    return t.div(
                        { className: "oidc-issuer m-b-10" },
                        t.div({ className: "txt-hint txt-sm" }, "Issuer / discovery URL"),
                        t.div(
                            { className: "inline-flex gap-xs" },
                            t.code(null, s.discovery),
                            app.components.copyButton(s.discovery),
                        ),
                    );
                },
                t.table(
                    { className: "oidc-clients-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Client"),
                            t.th(null, "Client ID"),
                            t.th(null, "Users from"),
                            t.th(null, "Type"),
                            t.th({ className: "col-meta" }, "Actions"),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.clients.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    state.isLoading
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div(
                                            { className: "txt-center txt-hint" },
                                            "No OAuth clients. Nothing signs in through this server and no background work runs.",
                                        ),
                                ),
                            );
                        }

                        return state.clients.map((c) =>
                            t.tr(
                                null,
                                t.td(
                                    null,
                                    t.div({ className: "txt-bold" }, c.name),
                                    c.disabled ? t.span({ className: "label sm danger" }, "disabled") : null,
                                ),
                                t.td(null, t.code(null, c.clientId)),
                                t.td(null, c.authCollection),
                                t.td(
                                    null,
                                    t.span({ className: "label sm" }, c.public ? "public" : "confidential"),
                                    " ",
                                    c.firstParty ? t.span({ className: "label sm success" }, "first party") : null,
                                ),
                                t.td(
                                    { className: "col-meta" },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        btn("ri-pencil-line", "Edit", () => openForm(c)),
                                        c.public ? null : btn("ri-key-2-line", "New secret", () => rotateSecret(c)),
                                        btn(c.disabled ? "ri-play-line" : "ri-pause-line", c.disabled ? "Enable" : "Disable", () => toggle(c)),
                                        btn("ri-delete-bin-7-line", "Delete", () => remove(c), "danger"),
                                    ),
                                ),
                            )
                        );
                    }),
                ),
                t.div(
                    { className: "flex gap-sm m-t-20 m-b-10" },
                    t.h6({ className: "m-r-auto" }, "Signing keys"),
                    t.button(
                        {
                            type: "button",
                            className: () => `btn sm secondary ${state.isRotating ? "loading" : ""}`,
                            disabled: () => state.isRotating || (state.status && !state.status.enabled),
                            onclick: rotateKeys,
                        },
                        t.i({ className: "ri-refresh-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Rotate keys"),
                    ),
                ),
                t.table(
                    { className: "oidc-keys-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Key ID"),
                            t.th(null, "Algorithm"),
                            t.th(null, "State"),
                            t.th(null, "Created"),
                            t.th(null, "Leaves the JWKS"),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.keys.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    t.div(
                                        { className: "txt-center txt-hint" },
                                        "No signing key yet. The first one is created when a client first signs a token.",
                                    ),
                                ),
                            );
                        }

                        return state.keys.map((k) =>
                            t.tr(
                                null,
                                t.td(null, t.code(null, k.kid)),
                                t.td(null, k.alg),
                                t.td(
                                    null,
                                    t.span({ className: `label sm ${keyStatusClass[k.status] || ""}` }, k.status),
                                    k.encrypted ? null : t.span({ className: "txt-warning txt-sm" }, " not encrypted (dev mode)"),
                                ),
                                t.td(null, app.components.formattedDate({ value: () => k.created, short: false })),
                                t.td(null, k.retiresAt ? app.components.formattedDate({ value: () => k.retiresAt, short: false }) : "-"),
                            )
                        );
                    }),
                ),
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
