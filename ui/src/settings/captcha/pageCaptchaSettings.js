// Fork-local (Claviger #91): CAPTCHA settings.

import { settingsSidebar } from "../settingsSidebar";

const PROVIDERS = [
    { value: "", label: "Off" },
    { value: "turnstile", label: "Cloudflare Turnstile" },
    { value: "hcaptcha", label: "hCaptcha" },
    { value: "recaptchaV2", label: "Google reCAPTCHA v2 (checkbox / invisible)" },
    { value: "recaptchaV3", label: "Google reCAPTCHA v3 (score)" },
];

const ACTIONS = [
    { key: "signUp", label: "Sign-up (creating an auth record)" },
    { key: "login", label: "Password sign-in (including the superuser login)" },
    { key: "otpRequest", label: "One-time password request" },
    { key: "resetRequest", label: "Password-reset and passkey-reset request" },
    { key: "verificationRequest", label: "Verification request" },
    { key: "passkeyLogin", label: "Passkey sign-in (login-begin)" },
];

export function pageCaptchaSettings() {
    app.store.title = "Captcha";

    const data = store({
        isLoading: false,
        isSaving: false,
        formSettings: null,
        // write-only: only sent when the user types a new one
        secretKey: "",
        hostnamesText: "",
        initSerialized: "null",
        get hasChanges() {
            return data.initSerialized != JSON.stringify([data.formSettings, data.secretKey, data.hostnamesText]);
        },
    });

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

        const captcha = JSON.parse(JSON.stringify(settings?.captcha || {}));
        captcha.actions = captcha.actions || {};
        delete captcha.secretKey;

        data.formSettings = { captcha };
        data.secretKey = "";
        data.hostnamesText = (captcha.hostnames || []).join(", ");
        data.initSerialized = JSON.stringify([data.formSettings, data.secretKey, data.hostnamesText]);
    }

    async function save() {
        if (data.isSaving || !data.hasChanges) {
            return;
        }

        data.isSaving = true;

        try {
            const captcha = JSON.parse(JSON.stringify(data.formSettings.captcha));
            captcha.minScore = Number(captcha.minScore) || 0;
            captcha.hostnames = data.hostnamesText.split(/[\s,]+/).filter(Boolean);
            if (data.secretKey) {
                captcha.secretKey = data.secretKey;
            }

            init(await app.pb.settings.update({ captcha }));

            app.toasts.success("Successfully saved captcha settings.");
        } catch (err) {
            app.checkApiError(err);
        }

        data.isSaving = false;
    }

    function reset() {
        const [form, secret, hostnames] = JSON.parse(data.initSerialized);
        data.formSettings = form;
        data.secretKey = secret;
        data.hostnamesText = hostnames;
    }

    function textField(id, label, get, set, extra = {}) {
        return t.div(
            { className: "field" },
            t.label({ htmlFor: id }, label),
            t.input({ id, name: id, value: get, oninput: (e) => set(e.target.value), ...extra }),
        );
    }

    function switchField(id, label, get, set) {
        return t.div(
            { className: "field" },
            t.input({
                id,
                name: id,
                type: "checkbox",
                className: "switch",
                checked: get,
                onchange: (e) => set(e.target.checked),
            }),
            t.label({ htmlFor: id }, t.span({ className: "txt" }, label)),
        );
    }

    return t.div(
        { pbEvent: "pageCaptchaSettings", className: "page page-captcha-settings" },
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

                    const c = data.formSettings.captcha;

                    return t.form(
                        {
                            pbEvent: "captchaSettingsForm",
                            className: "grid captcha-settings-form",
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
                                "Protect the unauthenticated sign-in and sign-up endpoints with a CAPTCHA. Nothing changes until you pick a provider and switch on at least one action. Clients send the token in the ",
                                t.code(null, "X-Captcha-Token"),
                                " header or the ",
                                t.code(null, "captchaToken"),
                                " body field; a rejected token is a 400 with the error code ",
                                t.code(null, "captcha_failed"),
                                ".",
                            ),
                        ),
                        t.div(
                            { className: "col-lg-6" },
                            t.div(
                                { className: "field" },
                                t.label({ htmlFor: "captcha_provider" }, "Provider"),
                                t.select(
                                    {
                                        id: "captcha_provider",
                                        value: () => c.provider || "",
                                        onchange: (e) => (c.provider = e.target.value),
                                    },
                                    PROVIDERS.map((p) => t.option({ value: p.value }, p.label)),
                                ),
                            ),
                        ),
                        () => {
                            if (!c.provider) {
                                return null;
                            }

                            return t.div(
                                { className: "col-lg-12 grid" },
                                t.div(
                                    { className: "col-lg-6" },
                                    textField("captcha_siteKey", "Site key (public)", () => c.siteKey || "", (v) => (c.siteKey = v), { required: true }),
                                ),
                                t.div(
                                    { className: "col-lg-6" },
                                    textField(
                                        "captcha_secretKey",
                                        "Secret key",
                                        () => data.secretKey,
                                        (v) => (data.secretKey = v),
                                        { type: "password", autocomplete: "new-password", placeholder: "Leave blank to keep the saved secret" },
                                    ),
                                ),
                                () => {
                                    if (c.provider !== "recaptchaV3") {
                                        return null;
                                    }
                                    return t.div(
                                        { className: "col-lg-6" },
                                        textField(
                                            "captcha_minScore",
                                            "Minimum score (0.0 - 1.0, default 0.5)",
                                            () => String(c.minScore || ""),
                                            (v) => (c.minScore = v),
                                            { type: "number", min: "0", max: "1", step: "0.1" },
                                        ),
                                    );
                                },
                                t.div(
                                    { className: "col-lg-12" },
                                    textField(
                                        "captcha_hostnames",
                                        "Allowed hostnames (optional, comma separated)",
                                        () => data.hostnamesText,
                                        (v) => (data.hostnamesText = v),
                                        { placeholder: "example.org, app.example.org" },
                                    ),
                                    t.div(
                                        { className: "field-help" },
                                        "When set, the provider must report that the challenge was solved on one of these hostnames. Leave empty to skip this check.",
                                    ),
                                ),
                                t.div(
                                    { className: "col-lg-12" },
                                    t.h6(null, "Require a captcha for"),
                                    ACTIONS.map((a) =>
                                        switchField(
                                            "captcha_action_" + a.key,
                                            a.label,
                                            () => !!c.actions[a.key],
                                            (v) => (c.actions[a.key] = v),
                                        )
                                    ),
                                ),
                                t.div(
                                    { className: "col-lg-12" },
                                    switchField(
                                        "captcha_failClosed",
                                        "Fail closed: refuse requests when the captcha provider is unreachable",
                                        () => !!c.failClosed,
                                        (v) => (c.failClosed = v),
                                    ),
                                    t.div(
                                        { className: "field-help" },
                                        "By default a provider outage lets requests through (logged as a warning and counted as outage_open), so a captcha outage cannot lock everyone out of sign-in. Rate limits still apply.",
                                    ),
                                ),
                            );
                        },
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
                    );
                },
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
