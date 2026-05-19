export function webauthnAccordion(collection) {
    const uniqueId = "webauthn_" + app.utils.randomString();

    const data = store({
        get config() {
            if (!collection.webauthn) {
                collection.webauthn = {
                    enabled: false,
                };
            }

            return collection.webauthn;
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
