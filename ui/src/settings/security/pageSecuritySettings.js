// Fork-local (Claviger #90): superuser TOTP (authenticator app) enrollment.
//
// There is no QR library in the UI dependencies, so the otpauth:// URI and the
// secret are shown as text: most authenticator apps accept "enter a setup key".

import { settingsSidebar } from "../settingsSidebar";

const COLLECTION = "_superusers";
const BASE = `/api/collections/${COLLECTION}/totp`;

export function pageSecuritySettings(route) {
    app.store.title = "Security";

    const state = store({
        isLoading: true,
        isSubmitting: false,
        status: { enabled: false, pending: false, backupCodesRemaining: 0 },

        // enrollment in progress
        secret: "",
        uri: "",
        // backup codes, shown once
        backupCodes: [],

        password: "",
        code: "",
        disableCode: "",
        regenCode: "",
    });

    async function loadStatus() {
        state.isLoading = true;
        try {
            state.status = await app.pb.send(BASE, { method: "GET", requestKey: "totp_status" });
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
        state.isLoading = false;
    }

    async function call(path, body) {
        if (state.isSubmitting) {
            return null;
        }
        state.isSubmitting = true;
        try {
            return await app.pb.send(BASE + path, { method: "POST", body });
        } catch (err) {
            app.checkApiError(err);
            return null;
        } finally {
            state.isSubmitting = false;
        }
    }

    async function begin() {
        const res = await call("/begin", { password: state.password });
        if (res) {
            state.secret = res.secret;
            state.uri = res.uri;
            state.password = "";
            state.code = "";
            state.status = { ...state.status, pending: true };
        }
    }

    async function confirm() {
        const res = await call("/confirm", { code: state.code });
        if (res) {
            state.backupCodes = res.backupCodes;
            state.secret = "";
            state.uri = "";
            state.code = "";
            app.toasts.success("Authenticator app enabled.");
            loadStatus();
        }
    }

    async function cancelPending() {
        const res = await call("/disable", {});
        if (res !== null) {
            state.secret = "";
            state.uri = "";
            loadStatus();
        }
    }

    async function disable() {
        const res = await call("/disable", { code: state.disableCode });
        if (res !== null) {
            state.disableCode = "";
            state.backupCodes = [];
            app.toasts.success("Authenticator app disabled.");
            loadStatus();
        }
    }

    async function regenerate() {
        const res = await call("/backup-codes", { code: state.regenCode });
        if (res) {
            state.regenCode = "";
            state.backupCodes = res.backupCodes;
            app.toasts.success("Generated new backup codes. The previous ones no longer work.");
            loadStatus();
        }
    }

    function field(id, label, key, extra = {}) {
        return t.div(
            { className: "field" },
            t.label({ htmlFor: id }, label),
            t.input({
                id,
                name: id,
                required: true,
                value: () => state[key],
                oninput: (e) => (state[key] = e.target.value),
                ...extra,
            }),
        );
    }

    function submitButton(text, className = "") {
        return t.button(
            {
                className: () => `btn ${className} ${state.isSubmitting ? "loading" : ""}`,
                disabled: () => state.isSubmitting,
            },
            t.span({ className: "txt" }, text),
        );
    }

    function backupCodesBlock() {
        return t.div(
            { className: "alert warning m-b-base" },
            t.div(
                { className: "content" },
                t.p(null, t.strong(null, "Save these backup codes now."), " They are shown only once. Each works a single time if you lose your authenticator app."),
                t.pre({ className: "txt-mono", id: "totp-backup-codes" }, () => state.backupCodes.join("\n")),
                t.button(
                    {
                        type: "button",
                        className: "btn sm secondary",
                        onclick: () => {
                            app.utils.copyToClipboard?.(state.backupCodes.join("\n"));
                        },
                    },
                    "Copy",
                ),
                " ",
                t.button(
                    { type: "button", className: "btn sm transparent", onclick: () => (state.backupCodes = []) },
                    "I saved them",
                ),
            ),
        );
    }

    function disabledView() {
        return t.div(
            null,
            () => {
                if (state.uri) {
                    return t.form(
                        {
                            className: "grid",
                            onsubmit: (e) => {
                                e.preventDefault();
                                confirm();
                            },
                        },
                        t.div(
                            { className: "col-12" },
                            t.p(
                                null,
                                "Add this account to your authenticator app: choose \"enter a setup key\" and paste the key below, or open the URI on your phone. (No QR code is rendered; this admin UI has no QR library.)",
                            ),
                            t.div({ className: "field" }, t.label(null, "Setup key"), t.input({ readOnly: true, value: () => state.secret })),
                            t.div({ className: "field" }, t.label(null, "otpauth URI"), t.input({ readOnly: true, value: () => state.uri })),
                        ),
                        t.div({ className: "col-12" }, field("totp_confirm_code", "First 6 digit code", "code", { autocomplete: "one-time-code", inputMode: "numeric" })),
                        t.div(
                            { className: "col-12" },
                            submitButton("Confirm and enable"),
                            " ",
                            t.button({ type: "button", className: "btn transparent", onclick: cancelPending }, "Cancel"),
                        ),
                    );
                }

                return t.form(
                    {
                        className: "grid",
                        onsubmit: (e) => {
                            e.preventDefault();
                            begin();
                        },
                    },
                    t.div(
                        { className: "col-12" },
                        t.p(
                            null,
                            "Enrolling is optional and only affects your own account. Once enabled, password sign-in also asks for a code from your authenticator app (or a backup code). Passkey sign-in is not affected.",
                        ),
                        field("totp_password", "Current password", "password", { type: "password", autocomplete: "current-password" }),
                    ),
                    t.div({ className: "col-12" }, submitButton("Set up authenticator app")),
                );
            },
        );
    }

    function enabledView() {
        return t.div(
            { className: "grid" },
            t.div(
                { className: "col-12" },
                t.p(
                    null,
                    "Authenticator app is ",
                    t.strong(null, "enabled"),
                    ". Unused backup codes: ",
                    t.strong(null, () => state.status.backupCodesRemaining),
                    ".",
                ),
            ),
            t.form(
                {
                    className: "col-12",
                    onsubmit: (e) => {
                        e.preventDefault();
                        regenerate();
                    },
                },
                t.h6(null, "Regenerate backup codes"),
                field("totp_regen_code", "Authenticator code (a backup code is not accepted here)", "regenCode", { autocomplete: "one-time-code", inputMode: "numeric" }),
                submitButton("Regenerate", "secondary"),
            ),
            t.form(
                {
                    className: "col-12",
                    onsubmit: (e) => {
                        e.preventDefault();
                        disable();
                    },
                },
                t.h6(null, "Disable"),
                field("totp_disable_code", "Authenticator code or backup code", "disableCode"),
                submitButton("Disable authenticator app", "danger"),
            ),
        );
    }

    loadStatus();

    return t.div(
        { pbEvent: "pageSecuritySettings", className: "page" },
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
                t.div({ className: "txt-lg m-b-sm" }, "Two-factor authentication (authenticator app)"),
                () => (state.backupCodes.length ? backupCodesBlock() : null),
                () => {
                    if (state.isLoading) {
                        return t.span({ className: "loader" });
                    }
                    return state.status.enabled ? enabledView() : disabledView();
                },
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
