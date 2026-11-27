// Fork-local (Claviger #92): modals of the "Single sign-on" section of the organizations page.
// The client secret is write-only: the API never returns it (hasSecret says whether one is set),
// so the field is always empty and an empty value keeps the stored secret.

const ROLE_CHOICES = ["admin", "member", "viewer"]; // single sign-on never assigns owner

const mappingToText = (m) => Object.entries(m || {}).map(([g, r]) => `${g} = ${r}`).join("\n");

function textToMapping(text) {
    const out = {};
    for (const part of (text || "").split(/[\n;]+/)) {
        if (!part.trim()) {
            continue;
        }
        const i = part.lastIndexOf("=");
        if (i <= 0) {
            throw new Error('Use "Group name = role", one per line.');
        }
        out[part.slice(0, i).trim()] = part.slice(i + 1).trim();
    }
    return out;
}

const words = (text) => (text || "").split(/[\s,]+/).map((v) => v.trim()).filter(Boolean);

function mount(modal) {
    document.body.appendChild(modal);
    app.modals.open(modal);
}

// info: { presets, types, callback }; conn: existing connection or null
export function openSsoConnectionModal(org, info, conn, onsave) {
    mount(connectionModal(org, info, conn, onsave));
}

export function openSsoDomainModal(org, connections, onsave) {
    mount(domainModal(org, connections, onsave));
}

function connectionModal(org, info, conn, onsave) {
    const formId = "sso_form_" + app.utils.randomString();
    const isNew = !conn;
    const cfg = conn?.config || {};

    const types = info.types?.length ? info.types : ["oidc"];

    const data = store({
        isSaving: false,
        error: "",
        preset: "",
        type: conn?.type || "oidc",
        name: conn?.name || "",
        issuer: conn?.issuer || "",
        clientId: conn?.clientId || "",
        clientSecret: "",
        scopes: (conn?.scopes || ["openid", "email", "profile"]).join(" "),
        groupsClaim: conn?.groupsClaim || "groups",
        roleMapping: mappingToText(conn?.roleMapping),
        defaultRole: conn?.defaultRole || "",
        jit: conn ? !!conn.jit : true,
        ssoRequired: !!conn?.ssoRequired,
        enabled: !!conn?.enabled,
        // SAML (config of the connection)
        samlMetadataUrl: cfg.metadataUrl || "",
        samlMetadataXml: cfg.metadataXml || "",
        samlEmailAttr: cfg.emailAttribute || "",
        samlNameAttr: cfg.nameAttribute || "",
        samlGroupsAttr: cfg.groupsAttribute || "",
    });

    const currentPreset = () => (info.presets || []).find((p) => p.id == data.preset);

    function applyPreset() {
        const p = currentPreset();
        if (!p) {
            return;
        }
        data.type = p.type || "oidc";
        data.name = data.name.trim() || p.name;
        data.scopes = (p.scopes || []).join(" ");
        data.groupsClaim = p.groupsClaim || "groups";
    }

    async function save() {
        data.isSaving = true;
        data.error = "";
        try {
            const body = {
                name: data.name.trim(),
                enabled: data.enabled,
                ssoRequired: data.ssoRequired,
                jit: data.jit,
                groupsClaim: data.groupsClaim.trim(),
                roleMapping: textToMapping(data.roleMapping),
                defaultRole: data.defaultRole,
            };
            if (isNew) {
                body.type = data.type;
            }
            if (data.type == "saml") {
                body.config = {
                    ...cfg,
                    metadataUrl: data.samlMetadataUrl.trim(),
                    metadataXml: data.samlMetadataXml.trim(),
                    emailAttribute: data.samlEmailAttr.trim(),
                    nameAttribute: data.samlNameAttr.trim(),
                    groupsAttribute: data.samlGroupsAttr.trim(),
                };
            } else {
                body.issuer = data.issuer.trim();
                body.clientId = data.clientId.trim();
                body.scopes = words(data.scopes);
                if (data.clientSecret) {
                    body.clientSecret = data.clientSecret;
                }
            }
            const url = `/api/orgs/${org.id}/sso/connections` + (isNew ? "" : "/" + conn.id);
            await app.pb.send(url, { method: isNew ? "POST" : "PATCH", body });
            app.toasts.success(isNew ? "Connection created. It stays disabled until you enable it." : "Connection saved.");
            onsave?.();
            app.modals.close();
        } catch (err) {
            data.error = err?.response?.message || err?.message || "Request failed.";
        }
        data.isSaving = false;
    }

    function field(id, label, input, hint) {
        return t.div(
            { className: "field" },
            input,
            t.label({ htmlFor: "sso_" + id }, label),
            hint ? t.div({ className: "field-help" }, hint) : null,
        );
    }

    function text(id, key, extra = {}) {
        return t.input({
            type: "text",
            id: "sso_" + id,
            value: () => data[key],
            oninput: (e) => data[key] = e.target.value,
            ...extra,
        });
    }

    function toggle(id, label, key, hint) {
        return t.div(
            { className: "field" },
            t.input({
                type: "checkbox",
                id: "sso_" + id,
                className: "switch",
                checked: () => data[key],
                onchange: (e) => data[key] = e.target.checked,
            }),
            t.label({ htmlFor: "sso_" + id }, label),
            hint ? t.div({ className: "field-help" }, hint) : null,
        );
    }

    function presetPicker() {
        return t.div(
            { className: "sso-preset" },
            field(
                "preset",
                "Start from a preset",
                t.select(
                    {
                        id: "sso_preset",
                        onchange: (e) => {
                            data.preset = e.target.value;
                            applyPreset();
                        },
                    },
                    t.option({ value: "" }, "Blank connection"),
                    (info.presets || []).map((p) => t.option({ value: p.id }, p.name)),
                ),
                "Fills in the scopes and the groups claim. The issuer is yours: replace the part in braces.",
            ),
            () => {
                const p = currentPreset();
                if (!p) {
                    return null;
                }
                return t.div(
                    { className: "sso-preset-notes m-b-10" },
                    p.issuerExample ? t.div(null, "Issuer: ", t.code(null, p.issuerExample)) : null,
                    t.ul(null, (p.notes || []).map((n) => t.li(null, n))),
                );
            },
        );
    }

    const oidcFields = () => [
        field("issuer", "Issuer", text("issuer", "issuer", { placeholder: () => currentPreset()?.issuerExample || "https://idp.example.org" }),
            "A pasted .../.well-known/openid-configuration address is trimmed to the issuer."),
        field("clientId", "Client ID", text("clientId", "clientId", { autocomplete: "off" })),
        field(
            "clientSecret",
            "Client secret",
            t.input({
                type: "password",
                id: "sso_clientSecret",
                autocomplete: "new-password",
                placeholder: conn?.hasSecret ? "A secret is stored. Leave empty to keep it." : "",
                value: () => data.clientSecret,
                oninput: (e) => data.clientSecret = e.target.value,
            }),
            "Write only: it is never shown again.",
        ),
        field("scopes", "Scopes", text("scopes", "scopes"), "Space separated; must include openid."),
    ];

    const samlFields = () => [
        field("samlMetadataUrl", "IdP metadata URL", text("samlMetadataUrl", "samlMetadataUrl", { placeholder: "https://idp.example.org/metadata" }),
            "Use the URL, or paste the XML below."),
        field(
            "samlMetadataXml",
            "IdP metadata XML",
            t.textarea({
                id: "sso_samlMetadataXml",
                rows: 4,
                value: () => data.samlMetadataXml,
                oninput: (e) => data.samlMetadataXml = e.target.value,
            }),
        ),
        field("samlEmailAttr", "Email attribute", text("samlEmailAttr", "samlEmailAttr"), "Empty uses the NameID or the default email attribute."),
        field("samlNameAttr", "Name attribute", text("samlNameAttr", "samlNameAttr")),
        field("samlGroupsAttr", "Groups attribute", text("samlGroupsAttr", "samlGroupsAttr")),
    ];

    return t.div(
        { pbEvent: "ssoConnectionModal", className: "modal sso-modal", onafterclose: (el) => el?.remove() },
        t.header({ className: "modal-header" }, t.h5(null, isNew ? "New connection" : "Edit connection")),
        t.div(
            { className: "modal-content" },
            t.form(
                {
                    id: formId,
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        save();
                    },
                },
                isNew ? presetPicker() : null,
                types.length > 1
                    ? field(
                        "type",
                        "Type",
                        t.select(
                            {
                                id: "sso_type",
                                disabled: !isNew,
                                onchange: (e) => data.type = e.target.value,
                            },
                            types.map((ty) => t.option({ value: ty, selected: () => data.type == ty }, ty == "oidc" ? "OpenID Connect" : ty == "saml" ? "SAML 2.0" : ty)),
                        ),
                        "The type of a connection cannot change.",
                    )
                    : null,
                field("name", "Name", text("name", "name", { required: true }), "Shown on the sign-in button."),
                () => data.type == "saml" ? samlFields() : oidcFields(),
                t.div(
                    { className: "field-help sso-callback" },
                    "Redirect URI to register at the provider: ",
                    t.code(null, info.callback || ""),
                    info.callback ? app.components.copyButton(info.callback) : null,
                ),
                field("groupsClaim", () => data.type == "saml" ? "Groups attribute for roles" : "Groups claim", text("groupsClaim", "groupsClaim"),
                    "A dotted path such as realm_access.roles reads a nested claim."),
                field(
                    "roleMapping",
                    "Group to role mapping",
                    t.textarea({
                        id: "sso_roleMapping",
                        rows: 3,
                        placeholder: "Parish Admins = admin",
                        value: () => data.roleMapping,
                        oninput: (e) => data.roleMapping = e.target.value,
                    }),
                    "One \"group = role\" per line. Single sign-on never assigns owner.",
                ),
                field(
                    "defaultRole",
                    "Role without a matching group",
                    t.select(
                        { id: "sso_defaultRole", onchange: (e) => data.defaultRole = e.target.value },
                        t.option({ value: "", selected: () => !data.defaultRole }, "member (default)"),
                        ROLE_CHOICES.map((r) => t.option({ value: r, selected: () => data.defaultRole == r }, r)),
                    ),
                ),
                toggle("jit", "Create accounts at first sign-in", "jit", "Off: only people who already have an account can sign in."),
                toggle(
                    "ssoRequired",
                    "Single sign-on required",
                    "ssoRequired",
                    "Refuses password and email-code sign-in for addresses at the verified domains. Passkeys and recovery still work. Check that you can sign in through the provider first.",
                ),
                toggle("enabled", "Enabled", "enabled", "A disabled connection routes nobody and requires nothing."),
                () => data.error ? t.div({ className: "alert danger" }, data.error) : null,
            ),
        ),
        t.footer(
            { className: "modal-footer" },
            t.button({ type: "button", className: "btn transparent", onclick: () => app.modals.close() }, t.span({ className: "txt" }, "Cancel")),
            t.button(
                {
                    type: "button",
                    className: () => `btn expanded ${data.isSaving ? "loading" : ""}`,
                    disabled: () => data.isSaving,
                    onclick: () => document.getElementById(formId)?.requestSubmit(),
                },
                t.span({ className: "txt" }, isNew ? "Create" : "Save"),
            ),
        ),
    );
}

function domainModal(org, connections, onsave) {
    const formId = "sso_domain_form_" + app.utils.randomString();
    const data = store({
        isSaving: false,
        error: "",
        domain: "",
        connection: connections[0]?.id || "",
        created: null,
    });

    async function save() {
        data.isSaving = true;
        data.error = "";
        try {
            data.created = await app.pb.send(`/api/orgs/${org.id}/sso/domains`, {
                method: "POST",
                body: { domain: data.domain.trim(), connection: data.connection },
            });
            onsave?.();
        } catch (err) {
            data.error = err?.response?.message || err?.message || "Request failed.";
        }
        data.isSaving = false;
    }

    function result() {
        const v = data.created.verification;
        return t.div(
            null,
            t.div(
                { className: "alert warning m-b-10" },
                `Publish this DNS record for ${data.created.domain}, then use "Verify now". It is shown here and in the table until the claim is verified.`,
            ),
            t.div(
                { className: "field" },
                t.input({ type: "text", id: "sso_txt_value", readOnly: true, value: v?.value || "" }),
                t.label({ htmlFor: "sso_txt_value" }, "TXT value"),
                t.div({ className: "field-addon" }, app.components.copyButton(v?.value || "")),
            ),
            v
                ? t.div(
                    { className: "txt-hint txt-sm" },
                    "At ",
                    t.code(null, v.name),
                    ", or if the DNS host cannot hold a second TXT record there, at ",
                    t.code(null, v.alt),
                    ".",
                )
                : null,
        );
    }

    return t.div(
        { pbEvent: "ssoDomainModal", className: "modal sso-modal", onafterclose: (el) => el?.remove() },
        t.header({ className: "modal-header" }, t.h5(null, "Claim a domain")),
        t.div({ className: "modal-content" }, () => {
            if (data.created) {
                return result();
            }
            return t.form(
                {
                    id: formId,
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        save();
                    },
                },
                t.div(
                    { className: "field" },
                    t.input({
                        type: "text",
                        id: "sso_domain",
                        required: true,
                        placeholder: "example.org",
                        value: () => data.domain,
                        oninput: (e) => data.domain = e.target.value,
                    }),
                    t.label({ htmlFor: "sso_domain" }, "Domain"),
                    t.div({ className: "field-help" }, "People with an address at this domain are sent to the connection once it is verified."),
                ),
                t.div(
                    { className: "field" },
                    t.select(
                        { id: "sso_domain_connection", onchange: (e) => data.connection = e.target.value },
                        connections.map((c) => t.option({ value: c.id, selected: () => data.connection == c.id }, c.name)),
                    ),
                    t.label({ htmlFor: "sso_domain_connection" }, "Connection"),
                ),
                () => data.error ? t.div({ className: "alert danger" }, data.error) : null,
            );
        }),
        t.footer(
            { className: "modal-footer" },
            t.button({ type: "button", className: "btn transparent", onclick: () => app.modals.close() }, t.span({ className: "txt" }, () => data.created ? "Done" : "Cancel")),
            t.button(
                {
                    type: "button",
                    hidden: () => !!data.created,
                    className: () => `btn expanded ${data.isSaving ? "loading" : ""}`,
                    disabled: () => data.isSaving,
                    onclick: () => document.getElementById(formId)?.requestSubmit(),
                },
                t.span({ className: "txt" }, "Claim domain"),
            ),
        ),
    );
}
