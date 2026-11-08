// Fork-local (Claviger #95): create an API key and show it once.

export function openApiKeyCreateModal(settings = { oncreated: null }) {
    const modal = apiKeyCreateModal(settings);
    document.body.appendChild(modal);
    app.modals.open(modal);
}

function apiKeyCreateModal(settings) {
    let modal;

    const uniqueId = "apikey_create_" + app.utils.randomString();

    const data = store({
        name: "",
        expires: "",
        allAccess: false,
        collections: [],
        scopes: {}, // "collectionName:read|write" -> true
        createdKey: "",
        isSubmitting: false,
    });

    async function loadCollections() {
        try {
            const list = await app.pb.collections.getFullList({ requestKey: uniqueId + "_cols" });
            data.collections = list
                .map((c) => c.name)
                .filter((n) => !["_apiKeys", "_mfas", "_otps", "_authOrigins", "_externalAuths", "_webauthnCredentials"].includes(n))
                .sort();
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    function selectedScopes() {
        if (data.allAccess) {
            return ["*"];
        }
        return Object.keys(data.scopes).filter((k) => data.scopes[k]).map((k) => {
            const i = k.lastIndexOf(":");
            return `collection:${k.slice(0, i)}:${k.slice(i + 1)}`;
        });
    }

    async function submit() {
        if (data.isSubmitting) {
            return;
        }

        data.isSubmitting = true;

        try {
            const body = { name: data.name, scopes: selectedScopes() };
            if (data.expires) {
                body.expires = new Date(data.expires).toISOString().replace("T", " ");
            }

            const result = await app.pb.send("/api/apikeys", { method: "POST", body, requestKey: uniqueId });

            data.isSubmitting = false;
            data.createdKey = result.key;

            settings.oncreated?.();
        } catch (err) {
            if (!err.isAbort) {
                data.isSubmitting = false;
                if (err.response?.data?.passkey?.code == "passkey_step_up_required") {
                    app.toasts.error("A recent passkey verification is required. Sign in again with your passkey and retry.");
                } else {
                    app.checkApiError(err);
                }
            }
        }
    }

    function scopeRow(name) {
        const box = (access) =>
            t.td(
                null,
                t.input({
                    type: "checkbox",
                    ariaLabel: `${name} ${access}`,
                    checked: () => !!data.scopes[`${name}:${access}`],
                    onchange: (e) => data.scopes[`${name}:${access}`] = e.target.checked,
                }),
            );
        return t.tr(null, t.td(null, name), box("read"), box("write"));
    }

    function form() {
        return t.form(
            {
                id: uniqueId,
                className: "modal-content",
                autocomplete: "off",
                onsubmit: (e) => {
                    e.preventDefault();
                    submit();
                },
            },
            t.div(
                { className: "grid" },
                t.div(
                    { className: "col-lg-12" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + "_name" }, "Name"),
                        t.input({
                            id: uniqueId + "_name",
                            type: "text",
                            required: true,
                            maxLength: 100,
                            value: () => data.name,
                            oninput: (e) => data.name = e.target.value,
                        }),
                    ),
                ),
                t.div(
                    { className: "col-lg-12" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + "_expires" }, "Expires (optional)"),
                        t.input({
                            id: uniqueId + "_expires",
                            type: "datetime-local",
                            value: () => data.expires,
                            oninput: (e) => data.expires = e.target.value,
                        }),
                    ),
                ),
                t.div(
                    { className: "col-lg-12" },
                    t.div(
                        { className: "field" },
                        t.input({
                            type: "checkbox",
                            id: uniqueId + "_all",
                            className: "switch",
                            checked: () => data.allAccess,
                            onchange: (e) => data.allAccess = e.target.checked,
                        }),
                        t.label({ htmlFor: uniqueId + "_all" }, "All access (*)"),
                    ),
                    t.div(
                        { className: "field-help" },
                        "Scopes only restrict: collection rules still apply as the key owner.",
                    ),
                ),
                t.div(
                    { className: "col-lg-12", hidden: () => data.allAccess },
                    t.table(
                        null,
                        t.thead(null, t.tr(null, t.th(null, "Collection"), t.th(null, "Read"), t.th(null, "Write"))),
                        t.tbody(null, () => data.collections.map(scopeRow)),
                    ),
                ),
            ),
        );
    }

    function reveal() {
        return t.div(
            { className: "modal-content" },
            t.div(
                { className: "alert warning" },
                t.div(
                    { className: "content" },
                    "Copy this key now. It is shown only once and cannot be retrieved later.",
                ),
            ),
            t.div(
                { className: "flex gap-10 m-t-sm" },
                t.code({ className: "txt-break" }, () => data.createdKey),
                app.components.copyButton(() => data.createdKey),
            ),
        );
    }

    modal = t.div(
        {
            pbEvent: "apiKeyCreateModal",
            className: "modal popup api-key-create-modal",
            onbeforeopen: () => {
                loadCollections();
            },
            onafterclose: (el) => {
                data.createdKey = ""; // drop the secret from memory
                el?.remove();
            },
        },
        t.header(
            { className: "modal-header" },
            t.h5({ className: "m-auto txt-center" }, () => data.createdKey ? "API key created" : "New API key"),
        ),
        () => data.createdKey ? reveal() : form(),
        t.footer(
            { className: "modal-footer" },
            () => {
                if (data.createdKey) {
                    return t.button(
                        { type: "button", className: "btn", onclick: () => app.modals.close(modal) },
                        t.span({ className: "txt" }, "Done"),
                    );
                }

                return [
                    t.button(
                        {
                            type: "button",
                            className: "btn transparent m-r-auto",
                            disabled: () => data.isSubmitting,
                            onclick: () => app.modals.close(modal),
                        },
                        t.span({ className: "txt" }, "Cancel"),
                    ),
                    t.button(
                        {
                            "html-form": uniqueId,
                            type: "submit",
                            className: () => `btn ${data.isSubmitting ? "loading" : ""}`,
                            disabled: () => data.isSubmitting,
                        },
                        t.span({ className: "txt" }, "Create key"),
                    ),
                ];
            },
        ),
    );

    return modal;
}
