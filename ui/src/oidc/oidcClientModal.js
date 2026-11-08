// Fork-local (Claviger #71): create/edit an OAuth client. A new or rotated
// client secret is shown exactly once.

window.app = window.app || {};
window.app.modals = window.app.modals || {};

window.app.modals.openOidcClientModal = function(client, settings = {}) {
    const modal = oidcClientModal(client, settings);
    document.body.appendChild(modal);
    app.modals.open(modal);
};

window.app.modals.openOidcClientSecret = function(clientId, secret) {
    const modal = oidcSecretModal(clientId, secret);
    document.body.appendChild(modal);
    app.modals.open(modal);
};

const lines = (text) => text.split(/[\s,]+/).map((v) => v.trim()).filter(Boolean);

function oidcClientModal(client, settings) {
    const isNew = !client?.id;

    const data = store({
        isSaving: false,
        collections: [],
        name: client?.name || "",
        authCollection: client?.authCollection || "users",
        public: !!client?.public,
        firstParty: !!client?.firstParty,
        requirePkce: client ? !!client.requirePkce : true,
        redirectUris: (client?.redirectUris || []).join("\n"),
        postLogoutUris: (client?.postLogoutUris || []).join("\n"),
        scopes: (client?.scopes || []).join(" "),
        accessTokenType: client?.accessTokenType || "jwt",
        signingAlg: client?.signingAlg || "RS256",
        clientId: "",
        secret: "",
        createdClientId: "",
        errors: {},
    });

    async function loadCollections() {
        try {
            const all = await app.pb.collections.getFullList({ filter: "type = 'auth'", requestKey: "oidc_cols" });
            data.collections = all.map((c) => c.name).filter((n) => n != "_superusers");
        } catch (err) {
            data.collections = ["users"];
        }
    }

    async function save() {
        data.isSaving = true;
        data.errors = {};

        const body = {
            name: data.name.trim(),
            authCollection: data.authCollection,
            firstParty: data.firstParty,
            requirePkce: data.public ? true : data.requirePkce,
            redirectUris: lines(data.redirectUris),
            postLogoutUris: lines(data.postLogoutUris),
            scopes: lines(data.scopes),
            accessTokenType: data.accessTokenType,
            signingAlg: data.signingAlg,
        };

        try {
            if (isNew) {
                body.public = data.public;
                if (data.clientId.trim()) {
                    body.clientId = data.clientId.trim();
                }
                const res = await app.pb.send("/api/oidc/clients", { method: "POST", body });
                settings.onsave?.(res);
                if (res.clientSecret) {
                    data.createdClientId = res.clientId;
                    data.secret = res.clientSecret;
                } else {
                    app.toasts.success("Client created.");
                    app.modals.close();
                }
            } else {
                const res = await app.pb.collection("_oauthClients").update(client.id, body);
                settings.onsave?.(res);
                app.toasts.success("Client saved.");
                app.modals.close();
            }
        } catch (err) {
            data.errors = err?.response?.data || {};
            if (!Object.keys(data.errors).length) {
                app.checkApiError(err);
            }
        }

        data.isSaving = false;
    }

    function field(id, label, input, hint) {
        return t.div(
            { className: () => `field ${data.errors[id] ? "error" : ""}` },
            input,
            t.label({ htmlFor: "oidc_" + id }, label),
            () => data.errors[id] ? t.div({ className: "field-help txt-danger" }, data.errors[id].message) : null,
            hint ? t.div({ className: "field-help" }, hint) : null,
        );
    }

    function toggleField(id, label, key, hint, disabled) {
        return t.div(
            { className: "field" },
            t.input({
                type: "checkbox",
                id: "oidc_" + id,
                className: "switch",
                checked: () => data[key],
                disabled: disabled || false,
                onchange: (e) => data[key] = e.target.checked,
            }),
            t.label({ htmlFor: "oidc_" + id }, label),
            hint ? t.div({ className: "field-help" }, hint) : null,
        );
    }

    return t.div(
        {
            pbEvent: "oidcClientModal",
            className: "modal oidc-modal",
            onbeforeopen: (el) => {
                loadCollections();
                settings.onbeforeopen?.(el);
            },
            onafteropen: (el) => settings.onafteropen?.(el),
            onafterclose: (el) => {
                settings.onafterclose?.(el);
                el?.remove();
            },
        },
        t.header({ className: "modal-header" }, t.h5(null, isNew ? "New OAuth client" : "Edit OAuth client")),
        t.div({ className: "modal-content" }, () => {
            if (data.secret) {
                return t.div(
                    null,
                    t.div(
                        { className: "alert warning m-b-10" },
                        "Copy the client secret now. It is shown only once and cannot be retrieved later (you can generate a new one).",
                    ),
                    t.div(
                        { className: "field" },
                        t.input({ type: "text", id: "oidc_created_id", readOnly: true, value: data.createdClientId }),
                        t.label({ htmlFor: "oidc_created_id" }, "Client ID"),
                        t.div({ className: "field-addon" }, app.components.copyButton(data.createdClientId)),
                    ),
                    t.div(
                        { className: "field" },
                        t.input({ type: "text", id: "oidc_created_secret", readOnly: true, value: data.secret }),
                        t.label({ htmlFor: "oidc_created_secret" }, "Client secret"),
                        t.div({ className: "field-addon" }, app.components.copyButton(data.secret)),
                    ),
                );
            }

            return t.form(
                {
                    id: "oidc_client_form",
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        save();
                    },
                },
                field(
                    "name",
                    "Name",
                    t.input({
                        type: "text",
                        id: "oidc_name",
                        required: true,
                        value: () => data.name,
                        oninput: (e) => data.name = e.target.value,
                    }),
                    "Shown to users on the consent screen.",
                ),
                field(
                    "authCollection",
                    "Users sign in from",
                    t.select(
                        {
                            id: "oidc_authCollection",
                            value: () => data.authCollection,
                            onchange: (e) => data.authCollection = e.target.value,
                        },
                        () => (data.collections.length ? data.collections : [data.authCollection]).map((n) => t.option({ value: n }, n)),
                    ),
                    "Each client is tied to one auth collection.",
                ),
                field(
                    "redirectUris",
                    "Redirect URIs (one per line)",
                    t.textarea({
                        id: "oidc_redirectUris",
                        rows: 3,
                        required: true,
                        placeholder: "https://app.example.com/callback",
                        value: () => data.redirectUris,
                        oninput: (e) => data.redirectUris = e.target.value,
                    }),
                    "Exact match. https only (http://localhost for development).",
                ),
                field(
                    "postLogoutUris",
                    "Post logout redirect URIs (one per line)",
                    t.textarea({
                        id: "oidc_postLogoutUris",
                        rows: 2,
                        value: () => data.postLogoutUris,
                        oninput: (e) => data.postLogoutUris = e.target.value,
                    }),
                ),
                field(
                    "scopes",
                    "Allowed scopes",
                    t.input({
                        type: "text",
                        id: "oidc_scopes",
                        placeholder: "openid profile email offline_access",
                        value: () => data.scopes,
                        oninput: (e) => data.scopes = e.target.value,
                    }),
                    "Space separated. Empty means openid profile email offline_access.",
                ),
                isNew
                    ? toggleField("public", "Public client (no secret)", "public", "Single-page and native apps. PKCE is always required.")
                    : null,
                toggleField("firstParty", "First party", "firstParty", "Skips the consent screen. Only for applications you run."),
                toggleField("requirePkce", "Require PKCE", "requirePkce", "Always on for public clients.", false),
                field(
                    "accessTokenType",
                    "Access tokens",
                    t.select(
                        {
                            id: "oidc_accessTokenType",
                            value: () => data.accessTokenType,
                            onchange: (e) => data.accessTokenType = e.target.value,
                        },
                        t.option({ value: "jwt" }, "JWT (5 minutes)"),
                        t.option({ value: "opaque" }, "Opaque (use introspection)"),
                    ),
                ),
                field(
                    "signingAlg",
                    "Token signing algorithm",
                    t.select(
                        {
                            id: "oidc_signingAlg",
                            value: () => data.signingAlg,
                            onchange: (e) => data.signingAlg = e.target.value,
                        },
                        t.option({ value: "RS256" }, "RS256 (default)"),
                        t.option({ value: "ES256" }, "ES256"),
                    ),
                ),
                isNew
                    ? field(
                        "clientId",
                        "Client ID (optional)",
                        t.input({
                            type: "text",
                            id: "oidc_clientId",
                            placeholder: "generated",
                            value: () => data.clientId,
                            oninput: (e) => data.clientId = e.target.value,
                        }),
                    )
                    : null,
            );
        }),
        t.footer(
            { className: "modal-footer" },
            t.button(
                { type: "button", className: "btn transparent", onclick: () => app.modals.close() },
                t.span({ className: "txt" }, () => data.secret ? "Done" : "Cancel"),
            ),
            t.button(
                {
                    type: "submit",
                    hidden: () => !!data.secret,
                    className: () => `btn expanded ${data.isSaving ? "loading" : ""}`,
                    disabled: () => data.isSaving,
                    onclick: () => document.getElementById("oidc_client_form")?.requestSubmit(),
                },
                t.span({ className: "txt" }, isNew ? "Create" : "Save"),
            ),
        ),
    );
}

function oidcSecretModal(clientId, secret) {
    return t.div(
        {
            pbEvent: "oidcClientSecretModal",
            className: "modal oidc-modal",
            onafterclose: (el) => el?.remove(),
        },
        t.header({ className: "modal-header" }, t.h5(null, "New client secret")),
        t.div(
            { className: "modal-content" },
            t.div(
                { className: "alert warning m-b-10" },
                "Copy it now; it is shown only once. The previous secret no longer works.",
            ),
            t.div(
                { className: "field" },
                t.input({ type: "text", id: "oidc_rotated_id", readOnly: true, value: clientId }),
                t.label({ htmlFor: "oidc_rotated_id" }, "Client ID"),
                t.div({ className: "field-addon" }, app.components.copyButton(clientId)),
            ),
            t.div(
                { className: "field" },
                t.input({ type: "text", id: "oidc_rotated_secret", readOnly: true, value: secret }),
                t.label({ htmlFor: "oidc_rotated_secret" }, "Client secret"),
                t.div({ className: "field-addon" }, app.components.copyButton(secret)),
            ),
        ),
        t.footer(
            { className: "modal-footer" },
            t.button(
                { type: "button", className: "btn", onclick: () => app.modals.close() },
                t.span({ className: "txt" }, "Done"),
            ),
        ),
    );
}
