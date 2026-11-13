// Fork-local (Claviger #83): SMS settings.

import { settingsSidebar } from "../settingsSidebar";

const PROVIDERS = [
    { value: "twilio", label: "Twilio" },
    { value: "webhook", label: "Generic webhook" },
];

const SECRETS = ["authToken", "apiKeySecret"];

export function pageSmsSettings() {
    app.store.title = "SMS";

    const data = store({
        isLoading: false,
        isSaving: false,
        isTesting: false,
        formSettings: null,
        // write-only: only sent when the user types a new one
        twilioAuthToken: "",
        twilioApiKeySecret: "",
        webhookSecret: "",
        testTo: "",
        initSerialized: "null",
        get hasChanges() {
            return data.initSerialized != serialize();
        },
    });

    function serialize() {
        return JSON.stringify([data.formSettings, data.twilioAuthToken, data.twilioApiKeySecret, data.webhookSecret]);
    }

    loadSettings();

    async function loadSettings() {
        data.isLoading = true;

        try {
            init(await app.pb.settings.getAll());
            data.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    function init(settings = {}) {
        app.store.settings = JSON.parse(JSON.stringify(settings));

        const sms = JSON.parse(JSON.stringify(settings?.sms || {}));
        sms.twilio = sms.twilio || {};
        sms.webhook = sms.webhook || {};
        for (const key of SECRETS) {
            delete sms.twilio[key];
        }
        delete sms.webhook.secret;

        data.formSettings = { sms };
        data.twilioAuthToken = "";
        data.twilioApiKeySecret = "";
        data.webhookSecret = "";
        data.initSerialized = serialize();
    }

    async function save() {
        if (data.isSaving || !data.hasChanges) {
            return;
        }

        data.isSaving = true;

        try {
            const sms = JSON.parse(JSON.stringify(data.formSettings.sms));
            // secrets are only sent when a new one was typed; an omitted one keeps the saved value
            if (data.twilioAuthToken) {
                sms.twilio.authToken = data.twilioAuthToken;
            }
            if (data.twilioApiKeySecret) {
                sms.twilio.apiKeySecret = data.twilioApiKeySecret;
            }
            if (data.webhookSecret) {
                sms.webhook.secret = data.webhookSecret;
            }

            init(await app.pb.settings.update({ sms }));

            app.toasts.success("Successfully saved SMS settings.");
        } catch (err) {
            app.checkApiError(err);
        }

        data.isSaving = false;
    }

    async function sendTest() {
        if (data.isTesting || !data.testTo) {
            return;
        }

        data.isTesting = true;

        try {
            await app.pb.send("/api/settings/test/sms", { method: "POST", body: { to: data.testTo } });
            app.toasts.success("Test SMS sent.");
        } catch (err) {
            app.checkApiError(err);
        }

        data.isTesting = false;
    }

    function reset() {
        const [form, token, keySecret, hookSecret] = JSON.parse(data.initSerialized);
        data.formSettings = form;
        data.twilioAuthToken = token;
        data.twilioApiKeySecret = keySecret;
        data.webhookSecret = hookSecret;
    }

    function textField(id, label, get, set, extra = {}) {
        return t.div(
            { className: "field" },
            t.label({ htmlFor: id }, label),
            t.input({ id, name: id, value: get, oninput: (e) => set(e.target.value), ...extra }),
        );
    }

    function secretField(id, label, get, set) {
        return textField(id, label, get, set, {
            type: "password",
            autocomplete: "new-password",
            placeholder: "Leave blank to keep the saved secret",
        });
    }

    function twilioFields(c) {
        return t.div(
            { className: "col-lg-12 grid" },
            t.div({ className: "col-lg-6" }, textField("sms_accountSid", "Account SID", () => c.twilio.accountSid || "", (v) => (c.twilio.accountSid = v), { required: true, placeholder: "AC..." })),
            t.div({ className: "col-lg-6" }, secretField("sms_authToken", "Auth token", () => data.twilioAuthToken, (v) => (data.twilioAuthToken = v))),
            t.div({ className: "col-lg-6" }, textField("sms_apiKeySid", "API key SID (optional, instead of the auth token)", () => c.twilio.apiKeySid || "", (v) => (c.twilio.apiKeySid = v), { placeholder: "SK..." })),
            t.div({ className: "col-lg-6" }, secretField("sms_apiKeySecret", "API key secret", () => data.twilioApiKeySecret, (v) => (data.twilioApiKeySecret = v))),
            t.div({ className: "col-lg-6" }, textField("sms_from", "From number (E.164)", () => c.twilio.from || "", (v) => (c.twilio.from = v), { placeholder: "+15005550006" })),
            t.div({ className: "col-lg-6" }, textField("sms_messagingServiceSid", "Messaging service SID (instead of a from number)", () => c.twilio.messagingServiceSid || "", (v) => (c.twilio.messagingServiceSid = v), { placeholder: "MG..." })),
        );
    }

    function webhookFields(c) {
        return t.div(
            { className: "col-lg-12 grid" },
            t.div({ className: "col-lg-8" }, textField("sms_webhookUrl", "Endpoint URL (https)", () => c.webhook.url || "", (v) => (c.webhook.url = v), { required: true, type: "url" })),
            t.div({ className: "col-lg-4" }, secretField("sms_webhookSecret", "Signing secret", () => data.webhookSecret, (v) => (data.webhookSecret = v))),
            t.div(
                { className: "col-lg-12" },
                t.div(
                    { className: "field-help" },
                    "Each message is POSTed as JSON ",
                    t.code(null, '{"to","body"}'),
                    " with ",
                    t.code(null, "X-Claviger-Timestamp"),
                    " and ",
                    t.code(null, "X-Claviger-Signature"),
                    " (v1=hex HMAC-SHA256 of timestamp.body).",
                ),
            ),
        );
    }

    return t.div(
        { pbEvent: "pageSmsSettings", className: "page page-sms-settings" },
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
            ),
            t.div(
                { className: "wrapper m-b-base" },
                () => {
                    if (data.isLoading) {
                        return t.div({ className: "block txt-center" }, t.span({ className: "loader lg" }));
                    }

                    const c = data.formSettings.sms;

                    return t.form(
                        {
                            pbEvent: "smsSettingsForm",
                            className: "grid sms-settings-form",
                            inert: () => data.isSaving,
                            onsubmit: (e) => {
                                e.preventDefault();
                                save();
                            },
                        },
                        t.div(
                            { className: "col-lg-12" },
                            t.p(
                                null,
                                "Optional text-message sender for transactional messages (for example account recovery links). Messages are sent only when a feature asks for one. You are responsible for carrier registration, consent and opt-out handling; see docs/sms.md.",
                            ),
                        ),
                        t.div(
                            { className: "col-lg-12" },
                            t.div(
                                { className: "field" },
                                t.input({
                                    id: "sms_enabled",
                                    name: "sms_enabled",
                                    type: "checkbox",
                                    className: "switch",
                                    checked: () => !!c.enabled,
                                    onchange: (e) => (c.enabled = e.target.checked),
                                }),
                                t.label({ htmlFor: "sms_enabled" }, t.span({ className: "txt" }, "Enable SMS")),
                            ),
                        ),
                        () => {
                            if (!c.enabled) {
                                return null;
                            }

                            return t.div(
                                { className: "col-lg-12 grid" },
                                t.div(
                                    { className: "col-lg-6" },
                                    t.div(
                                        { className: "field" },
                                        t.label({ htmlFor: "sms_provider" }, "Provider"),
                                        t.select(
                                            {
                                                id: "sms_provider",
                                                value: () => c.provider || "",
                                                onchange: (e) => (c.provider = e.target.value),
                                            },
                                            [{ value: "", label: "Select a provider" }, ...PROVIDERS].map((p) => t.option({ value: p.value }, p.label)),
                                        ),
                                    ),
                                ),
                                () => (c.provider === "twilio" ? twilioFields(c) : null),
                                () => (c.provider === "webhook" ? webhookFields(c) : null),
                            );
                        },
                        t.div(
                            { className: "col-lg-4" },
                            textField("sms_defaultRegion", "Default region (optional)", () => c.defaultRegion || "", (v) => (c.defaultRegion = v.trim().toUpperCase()), {
                                placeholder: "US",
                                maxlength: 2,
                            }),
                            t.div(
                                { className: "field-help" },
                                "ISO country code used for household phone numbers without a + prefix, for example US or GB. Leave blank to text only numbers that start with +.",
                            ),
                        ),
                        t.div({ className: "col-lg-12" }, t.hr()),
                        t.div(
                            { className: "col-lg-12" },
                            t.div(
                                { className: "flex" },
                                t.div({ className: "m-r-auto" }),
                                t.button(
                                    {
                                        hidden: () => !data.hasChanges,
                                        type: "button",
                                        className: "btn transparent secondary",
                                        onclick: reset,
                                    },
                                    t.span({ className: "txt" }, "Cancel"),
                                ),
                                t.button(
                                    {
                                        className: () => `btn expanded-lg ${data.isSaving ? "loading" : ""}`,
                                        disabled: () => !data.hasChanges || data.isSaving,
                                    },
                                    t.span({ className: "txt" }, "Save changes"),
                                ),
                            ),
                        ),
                        t.div(
                            { className: "col-lg-12" },
                            t.h6(null, "Send a test message"),
                            t.p(null, "Uses the saved settings. Save first if you changed anything."),
                            t.div(
                                { className: "flex" },
                                textField("sms_testTo", "Phone number (E.164)", () => data.testTo, (v) => (data.testTo = v), { placeholder: "+14155550123", type: "tel" }),
                                t.button(
                                    {
                                        type: "button",
                                        className: () => `btn secondary m-l-base ${data.isTesting ? "loading" : ""}`,
                                        disabled: () => data.isTesting || !data.testTo || data.hasChanges || !c.enabled,
                                        onclick: sendTest,
                                    },
                                    t.span({ className: "txt" }, "Send test"),
                                ),
                            ),
                        ),
                    );
                },
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
