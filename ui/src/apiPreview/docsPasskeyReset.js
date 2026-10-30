export function docsPasskeyReset(collection) {
    const baseURL = app.utils.getApiExampleURL();

    const actionTabs = [
        { title: "Request passkey reset", content: request },
        { title: "Confirm passkey reset", content: confirm },
    ];

    const data = store({
        activeActionIndex: 0,
    });

    return t.div(
        {
            pbEvent: "apiPreviewPasskeyReset",
            className: "content",
        },
        t.p(
            null,
            `Self-service passkey recovery for ${collection.name} records. Sends an email with a `,
            "reset token that the user can exchange for a fresh WebAuthn registration ceremony.",
        ),
        t.p(
            null,
            "On successful confirmation ",
            t.strong(null, "all previously registered passkeys"),
            " for the account are removed and the newly registered credential becomes the only valid passkey ",
            "(the reset token is also invalidated).",
        ),
        app.components.codeBlockTabs({
            className: "sdk-examples m-t-sm",
            historyKey: "pbLastSDK",
            tabs: [
                {
                    title: "curl",
                    language: "bash",
                    value: `
                        # Request passkey reset (always returns 204 to prevent email enumeration)
                        curl -X POST \\
                          -H 'Content-Type:application/json' \\
                          -d '{ "email":"test@example.com" }' \\
                          '${baseURL}/api/collections/${collection.name}/request-passkey-reset'

                        # Confirm: step 1 - exchange the email token for a WebAuthn registration challenge
                        curl -X POST \\
                          -H 'Content-Type:application/json' \\
                          -d '{ "token":"RESET_TOKEN" }' \\
                          '${baseURL}/api/collections/${collection.name}/confirm-passkey-reset/begin'

                        # Confirm: step 2 - complete the WebAuthn ceremony and replace all passkeys
                        curl -X POST \\
                          -H 'Content-Type:application/json' \\
                          -d '{ "token":"RESET_TOKEN", "sessionToken":"...", "name":"...", "id":"...", "rawId":"...", "type":"public-key", "response": { "attestationObject":"...", "clientDataJSON":"..." } }' \\
                          '${baseURL}/api/collections/${collection.name}/confirm-passkey-reset/finish'
                    `,
                },
            ],
        }),
        t.nav(
            { className: "btns m-t-base m-b-sm" },
            () => {
                return actionTabs.map((tab, i) => {
                    return t.button({
                        type: "button",
                        className: () => `btn sm expanded ${data.activeActionIndex == i ? "active" : "secondary"}`,
                        textContent: () => tab.title,
                        onclick: () => (data.activeActionIndex = i),
                    });
                });
            },
        ),
        () => actionTabs[data.activeActionIndex]?.content?.(collection),
    );
}

function request(collection) {
    return [
        t.div({ className: "block" }, t.strong(null, "API details")),
        t.div(
            { className: "alert success api-preview-alert" },
            t.span({ className: "label method" }, "POST"),
            t.span({ className: "path" }, `/api/collections/${collection.name}/request-passkey-reset`),
        ),
        t.table(
            { className: "api-preview-table body-params" },
            t.thead(
                null,
                t.tr(
                    null,
                    t.th({ className: "min-width txt-primary" }, "Body params"),
                    t.th({ className: "min-width" }, "Type"),
                    t.th(null, "Description"),
                ),
            ),
            t.tbody(
                null,
                t.tr(
                    null,
                    t.td({ className: "min-width" }, "email ", t.em(null, "(required)")),
                    t.td({ className: "min-width" }, t.span({ className: "label" }, "String")),
                    t.td(
                        null,
                        "The auth record email to receive the passkey reset link (if a matching record exists).",
                    ),
                ),
            ),
        ),
        t.p(
            { className: "txt-hint m-t-xs" },
            "Always returns ",
            t.code(null, "204"),
            " to prevent email enumeration.",
        ),
    ];
}

function confirm(collection) {
    return [
        t.div({ className: "block" }, t.strong(null, "API details")),
        t.div(
            { className: "alert success api-preview-alert" },
            t.span({ className: "label method" }, "POST"),
            t.span({ className: "path" }, `/api/collections/${collection.name}/confirm-passkey-reset/begin`),
        ),
        t.p(
            { className: "txt-hint" },
            "Body: ",
            t.code(null, "{ \"token\": \"RESET_TOKEN\" }"),
            ". Returns ",
            t.code(null, "{ options, sessionToken }"),
            " — use ",
            t.code(null, "options.publicKey"),
            " with ",
            t.code(null, "navigator.credentials.create()"),
            ".",
        ),
        t.div(
            { className: "alert success api-preview-alert m-t-sm" },
            t.span({ className: "label method" }, "POST"),
            t.span({ className: "path" }, `/api/collections/${collection.name}/confirm-passkey-reset/finish`),
        ),
        t.table(
            { className: "api-preview-table body-params" },
            t.thead(
                null,
                t.tr(
                    null,
                    t.th({ className: "min-width txt-primary" }, "Body params"),
                    t.th({ className: "min-width" }, "Type"),
                    t.th(null, "Description"),
                ),
            ),
            t.tbody(
                null,
                t.tr(
                    null,
                    t.td({ className: "min-width" }, "token ", t.em(null, "(required)")),
                    t.td({ className: "min-width" }, t.span({ className: "label" }, "String")),
                    t.td(null, "The reset token from the email."),
                ),
                t.tr(
                    null,
                    t.td({ className: "min-width" }, "sessionToken ", t.em(null, "(required)")),
                    t.td({ className: "min-width" }, t.span({ className: "label" }, "String")),
                    t.td(null, "The session token returned by ", t.code(null, "confirm-passkey-reset/begin"), "."),
                ),
                t.tr(
                    null,
                    t.td({ className: "min-width" }, "name"),
                    t.td({ className: "min-width" }, t.span({ className: "label" }, "String")),
                    t.td(null, "Optional human-readable label for the new credential."),
                ),
                t.tr(
                    null,
                    t.td({ className: "min-width" }, "id, rawId, type, response.*"),
                    t.td({ className: "min-width" }, t.span({ className: "label" }, "WebAuthn")),
                    t.td(
                        null,
                        "Attestation produced by ",
                        t.code(null, "navigator.credentials.create()"),
                        " — base64url-encode all ",
                        t.code(null, "ArrayBuffer"),
                        " fields.",
                    ),
                ),
            ),
        ),
        t.p(
            { className: "txt-hint m-t-xs" },
            "On success all existing passkeys for the record are deleted, the new credential is saved, ",
            "and the reset token is rotated to be single-use.",
        ),
    ];
}
