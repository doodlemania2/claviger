export function webauthnAccordion(collection) {
    const uniqueId = "webauthn_" + app.utils.randomString();

    const data = store({
        get config() {
            if (!collection.webauthn) {
                collection.webauthn = {
                    enabled: false,
                    discoverable: false,
                };
            }

            return collection.webauthn;
        },

        // passkeys are the only sign-in method (mirrors the server rule that
        // then requires a recovery path)
        get passkeyOnly() {
            return (
                !!collection.webauthn?.enabled &&
                !collection.passwordAuth?.enabled &&
                !collection.oauth2?.enabled &&
                !collection.otp?.enabled
            );
        },
    });

    return t.details(
        {
            pbEvent: "webauthnAccordion",
            name: "auth-methods",
            className: "accordion webauthn-accordion",
        },
        t.summary(
            null,
            t.i({ className: "ri-fingerprint-line", ariaHidden: true }),
            t.span({ className: "txt", textContent: "WebAuthn / Passkeys" }),
            t.span({
                className: () => `label m-l-auto ${data.config.enabled ? "success" : ""}`,
                textContent: () => (data.config.enabled ? "Enabled" : "Disabled"),
            }),
            () => {
                if (!app.store.errors?.webauthn) {
                    return;
                }

                return t.i({
                    className: "ri-error-warning-fill txt-danger",
                    ariaDescription: app.attrs.tooltip("Has errors", "left"),
                });
            },
        ),
        t.div(
            { className: "grid sm" },
            t.div(
                { className: "col-sm-12" },
                t.div(
                    { className: "field" },
                    t.input({
                        type: "checkbox",
                        id: uniqueId + ".enabled",
                        name: "webauthn.enabled",
                        className: "switch",
                        checked: () => data.config.enabled,
                        onchange: (e) => {
                            data.config.enabled = e.target.checked;
                        },
                    }),
                    t.label({
                        htmlFor: uniqueId + ".enabled",
                        textContent: "Enable",
                    }),
                ),
            ),
            t.div(
                { className: "col-sm-12", hidden: () => !data.config.enabled },
                t.div(
                    { className: "field" },
                    t.input({
                        type: "checkbox",
                        id: uniqueId + ".discoverable",
                        name: "webauthn.discoverable",
                        className: "switch",
                        checked: () => !!data.config.discoverable,
                        onchange: (e) => {
                            data.config.discoverable = e.target.checked;
                        },
                    }),
                    t.label({
                        htmlFor: uniqueId + ".discoverable",
                        textContent: "Usernameless sign-in (autofill)",
                    }),
                ),
                t.div(
                    { className: "content txt-hint txt-sm" },
                    t.p(
                        null,
                        "Let users pick a saved passkey from the browser autofill without typing an email or username. New passkeys are created as discoverable credentials; passkeys registered before turning this on keep working with an email or username.",
                    ),
                ),
            ),
            t.div(
                {
                    className: "col-sm-12",
                    hidden: () => !data.passkeyOnly,
                },
                t.div(
                    { className: "alert warning" },
                    t.p(
                        null,
                        "Passkeys are the only sign-in method. Users recover a lost passkey through the emailed reset link, so the ",
                        t.code(null, "email"),
                        " field must be required (and a sender address set in the application settings). Records are created with a password the user never needs.",
                    ),
                ),
            ),
            t.div(
                { className: "col-sm-12" },
                t.div(
                    { className: "content txt-hint txt-sm" },
                    t.p(
                        null,
                        "Allow users to register and authenticate with passkeys (hardware security keys, Touch ID, Face ID, Windows Hello, etc.).",
                    ),
                    t.p(
                        null,
                        "Passkey credentials are stored in the ",
                        t.code(null, "_webauthnCredentials"),
                        " system collection.",
                    ),
                ),
            ),
        ),
    );
}
