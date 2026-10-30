import PocketBase, { getTokenPayload } from "pocketbase";

// minimal base64url <-> ArrayBuffer helpers (mirrors apis/passkeys_page.go).
const b64uToBuf = (s) => {
    s = String(s).replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    const bin = atob(s);
    const buf = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
    return buf.buffer;
};

const bufToB64u = (buf) => {
    const bytes = new Uint8Array(buf);
    let bin = "";
    for (let i = 0; i < bytes.byteLength; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

const decodePublicKeyOptions = (opts) => {
    if (!opts) return opts;
    if (opts.challenge) opts.challenge = b64uToBuf(opts.challenge);
    if (opts.user && opts.user.id) opts.user.id = b64uToBuf(opts.user.id);
    if (Array.isArray(opts.allowCredentials)) {
        opts.allowCredentials = opts.allowCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
    }
    if (Array.isArray(opts.excludeCredentials)) {
        opts.excludeCredentials = opts.excludeCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
    }
    return opts;
};

const encodeAttestation = (cred) => ({
    id: cred.id,
    rawId: bufToB64u(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    response: {
        attestationObject: bufToB64u(cred.response.attestationObject),
        clientDataJSON: bufToB64u(cred.response.clientDataJSON),
    },
});

export function pageConfirmPasskeyReset(route) {
    const token = route.params?.token || "";
    const tokenPayload = getTokenPayload(token);

    if (!tokenPayload.email || !tokenPayload.collectionId) {
        app.toasts.error("Invalid or expired passkey reset token.");
        window.location.hash = "#/";
        return;
    }

    app.store.title = "Confirm passkey reset";

    const data = store({
        credentialName: "",
        isSubmitting: false,
        isSuccess: false,
    });

    // use a custom client to avoid interfering with the superuser state
    const client = new PocketBase(app.pb.baseURL);

    async function submit() {
        if (data.isSubmitting) {
            return;
        }

        if (!window.PublicKeyCredential || !navigator.credentials || !navigator.credentials.create) {
            app.toasts.error("This browser does not support WebAuthn/passkeys.");
            return;
        }

        data.isSubmitting = true;

        try {
            const basePath = client.baseURL.replace(/\/+$/, "")
                + "/api/collections/"
                + encodeURIComponent(tokenPayload.collectionId);

            // begin
            const beginRes = await fetch(basePath + "/confirm-passkey-reset/begin", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ token }),
            });
            const beginBody = await beginRes.json();
            if (!beginRes.ok) {
                throw new Error(beginBody?.message || "Failed to start passkey reset.");
            }

            let publicKey = beginBody.options || beginBody.publicKey || beginBody;
            if (publicKey && publicKey.publicKey) publicKey = publicKey.publicKey;
            publicKey = decodePublicKeyOptions(publicKey);

            const cred = await navigator.credentials.create({ publicKey });

            // finish
            const finishRes = await fetch(basePath + "/confirm-passkey-reset/finish", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify(
                    Object.assign(
                        {
                            token,
                            sessionToken: beginBody.sessionToken,
                            name: data.credentialName || "",
                        },
                        encodeAttestation(cred),
                    ),
                ),
            });
            const finishBody = await finishRes.json().catch(() => ({}));
            if (!finishRes.ok) {
                throw new Error(finishBody?.message || "Failed to finish passkey reset.");
            }

            data.isSuccess = true;
        } catch (err) {
            app.toasts.error(err?.message || "Passkey reset failed.");
        }

        data.isSubmitting = false;
    }

    return t.div(
        {
            pbEvent: "pageConfirmPasskeyReset",
            className: "wrapper sm m-auto p-b-base",
        },
        t.header(
            { className: "txt-center m-b-base" },
            t.img({ className: "main-logo", src: () => app.store.mainLogo, ariaHidden: true, alt: "App logo" }),
            t.h5({ className: "m-t-10" }, () => app.store.title),
        ),
        () => {
            if (data.isSuccess) {
                return t.div(
                    { pbEvent: "confirmPasskeyResetAlert", className: "alert success txt-center" },
                    t.p(null, "Your new passkey was registered successfully."),
                    t.p(
                        null,
                        "All previously registered passkeys for this account have been removed. ",
                        "You can now sign in using your new passkey.",
                    ),
                );
            }

            return t.form(
                {
                    pbEvent: "confirmPasskeyResetForm",
                    className: "grid confirm-passkey-reset-form",
                    onsubmit: (e) => {
                        e.preventDefault();
                        submit();
                    },
                },
                t.div(
                    { className: "col-12" },
                    t.div(
                        { className: "content txt-center m-b-sm" },
                        "Register a new passkey for ",
                        t.strong(null, tokenPayload.email),
                        ".",
                        t.br(),
                        t.small(
                            { className: "txt-hint" },
                            "Any previously registered passkeys for this account will be removed.",
                        ),
                    ),
                    t.div(
                        { className: "fields" },
                        t.div(
                            { className: "field" },
                            t.label({ htmlFor: "credentialName" }, "Passkey name (optional)"),
                            t.input({
                                id: "credentialName",
                                name: "credentialName",
                                autofocus: true,
                                autocomplete: "off",
                                type: "text",
                                placeholder: "e.g. iPhone, YubiKey",
                                value: () => data.credentialName,
                                oninput: (e) => (data.credentialName = e.target.value),
                            }),
                        ),
                    ),
                ),
                t.div(
                    { className: "col-12" },
                    t.button(
                        {
                            className: () => `btn lg block ${data.isSubmitting ? "loading" : ""}`,
                            disabled: () => data.isSubmitting,
                        },
                        t.span({ className: "txt" }, "Register new passkey"),
                    ),
                ),
            );
        },
    );
}
