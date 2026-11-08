// Fork-local (Claviger #77): create/edit webhook endpoint modal. A newly
// created or rotated secret is shown here exactly once.

window.app = window.app || {};
window.app.modals = window.app.modals || {};

window.app.modals.openWebhookModal = function(endpoint, settings = {}) {
    const modal = webhookModal(endpoint, settings);
    document.body.appendChild(modal);
    app.modals.open(modal);
};

function webhookModal(endpoint, settings) {
    const isNew = !endpoint?.id;

    const data = store({
        isSaving: false,
        url: endpoint?.url || "",
        description: endpoint?.description || "",
        eventsText: (endpoint?.events || ["*"]).join("\n"),
        enabled: endpoint ? !!endpoint.enabled : true,
        secret: "",
        errors: {},
    });

    function parseEvents() {
        return data.eventsText.split(/[\s,]+/).map((v) => v.trim()).filter(Boolean);
    }

    async function save() {
        data.isSaving = true;
        data.errors = {};

        const body = {
            url: data.url.trim(),
            description: data.description.trim(),
            events: parseEvents(),
            enabled: data.enabled,
        };

        try {
            if (isNew) {
                const res = await app.pb.send("/api/webhooks", { method: "POST", body });
                data.secret = res.secret;
                settings.onsave?.(res.endpoint);
                app.toasts.success("Webhook created.");
            } else {
                const res = await app.pb.collection("_webhooks").update(endpoint.id, body);
                settings.onsave?.(res);
                app.toasts.success("Webhook saved.");
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
            t.label({ htmlFor: "webhook_" + id }, label),
            () => data.errors[id] ? t.div({ className: "field-help txt-danger" }, data.errors[id].message) : null,
            hint ? t.div({ className: "field-help" }, hint) : null,
        );
    }

    return t.div(
        {
            pbEvent: "webhookModal",
            className: "modal webhook-modal",
            onbeforeopen: (el) => settings.onbeforeopen?.(el),
            onafteropen: (el) => settings.onafteropen?.(el),
            onafterclose: (el) => {
                settings.onafterclose?.(el);
                el?.remove();
            },
        },
        t.header({ className: "modal-header" }, t.h5(null, isNew ? "New webhook" : "Edit webhook")),
        t.div({ className: "modal-content" }, () => {
            if (data.secret) {
                return t.div(
                    null,
                    t.div(
                        { className: "alert warning m-b-10" },
                        "Copy the signing secret now. It is shown only once and cannot be retrieved later (you can rotate it).",
                    ),
                    t.div(
                        { className: "field" },
                        t.input({ type: "text", id: "webhook_secret", readOnly: true, value: data.secret }),
                        t.label({ htmlFor: "webhook_secret" }, "Signing secret"),
                        t.div({ className: "field-addon" }, app.components.copyButton(data.secret)),
                    ),
                );
            }

            return t.form(
                {
                    id: "webhook_form",
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        save();
                    },
                },
                field(
                    "url",
                    "URL (https)",
                    t.input({
                        type: "url",
                        id: "webhook_url",
                        required: true,
                        placeholder: "https://example.com/hooks/claviger",
                        value: () => data.url,
                        oninput: (e) => data.url = e.target.value,
                    }),
                ),
                field(
                    "description",
                    "Description",
                    t.input({
                        type: "text",
                        id: "webhook_description",
                        value: () => data.description,
                        oninput: (e) => data.description = e.target.value,
                    }),
                ),
                field(
                    "events",
                    "Events (one per line)",
                    t.textarea({
                        id: "webhook_events",
                        rows: 4,
                        required: true,
                        value: () => data.eventsText,
                        oninput: (e) => data.eventsText = e.target.value,
                    }),
                    "Use collection:create|update|delete, collection:* , *:delete or * for everything. Example: users:create",
                ),
                t.div(
                    { className: "field" },
                    t.input({
                        type: "checkbox",
                        id: "webhook_enabled",
                        className: "switch",
                        checked: () => data.enabled,
                        onchange: (e) => data.enabled = e.target.checked,
                    }),
                    t.label({ htmlFor: "webhook_enabled" }, "Enabled"),
                ),
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
                    onclick: () => document.getElementById("webhook_form")?.requestSubmit(),
                },
                t.span({ className: "txt" }, isNew ? "Create" : "Save"),
            ),
        ),
    );
}

// shown after a rotate: same once-only presentation
window.app.modals.openWebhookSecret = function(secret) {
    const modal = secretModal(secret);
    document.body.appendChild(modal);
    app.modals.open(modal);
};

function secretModal(secret) {
    return t.div(
        {
            pbEvent: "webhookSecretModal",
            className: "modal webhook-modal",
            onafterclose: (el) => el?.remove(),
        },
        t.header({ className: "modal-header" }, t.h5(null, "New signing secret")),
        t.div(
            { className: "modal-content" },
            t.div(
                { className: "alert warning m-b-10" },
                "Copy it now; it is shown only once. The previous secret keeps working for 24 hours and deliveries carry both signatures meanwhile.",
            ),
            t.div(
                { className: "field" },
                t.input({ type: "text", id: "webhook_rotated_secret", readOnly: true, value: secret }),
                t.label({ htmlFor: "webhook_rotated_secret" }, "Signing secret"),
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
