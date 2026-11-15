// Fork-local (Claviger #88, 88-5): shared pieces of the "Data protection" admin pages: the sidebar,
// the page frame, error handling and the passkey step-up that placing a hold and arming a live
// retention policy need. Every page here is for superusers and calls routes that already exist.

export const GDPR_LINKS = [
    { href: "#/gdpr/data-map", icon: "ri-node-tree", label: "Data map" },
    { href: "#/gdpr/requests", icon: "ri-inbox-archive-line", label: "Data requests" },
    { href: "#/gdpr/holds", icon: "ri-lock-2-line", label: "Legal holds" },
    { href: "#/gdpr/retention", icon: "ri-timer-line", label: "Retention policies" },
    { href: "#/consents", icon: "ri-file-shield-2-line", label: "Consents and history" },
];

export function message(err) {
    const base = err?.response?.message || err?.message || "Request failed.";
    const fields = Object.entries(err?.response?.data || {})
        .map(([k, v]) => v?.message ? `${k}: ${v.message}` : "")
        .filter(Boolean);
    return fields.length ? `${base} ${fields.join(" ")}` : base;
}

export function isStepUpRequired(err) {
    return err?.status == 403 && err?.response?.data?.passkey?.code == "passkey_step_up_required";
}

// ---- passkey step-up (the same ceremony as pb.collection(name).stepUpPasskey() of the hosted pages) ----

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

function decodeOptions(opts) {
    if (opts?.challenge) opts.challenge = b64uToBuf(opts.challenge);
    if (Array.isArray(opts?.allowCredentials)) {
        opts.allowCredentials = opts.allowCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
    }
    return opts;
}

/**
 * Asks the browser for a passkey assertion of the signed-in superuser and swaps the session token
 * for one that carries it. Rejects with a readable message when the account has no passkey.
 */
export async function stepUp() {
    if (!window.PublicKeyCredential || !navigator.credentials?.get) {
        throw new Error("This browser does not support passkeys.");
    }
    const name = app.pb.authStore.record?.collectionName || "_superusers";
    const base = `/api/collections/${encodeURIComponent(name)}/auth-with-webauthn`;

    let begin;
    try {
        begin = await app.pb.send(base + "/step-up-begin", { method: "POST" });
    } catch (err) {
        const hint = err?.status == 403
            ? " Turn on passkeys for the superusers collection and register one on /passkeys?collection=_superusers."
            : "";
        throw new Error(message(err) + hint);
    }

    let publicKey = begin.options || begin.publicKey || begin;
    if (publicKey?.publicKey) publicKey = publicKey.publicKey;
    const cred = await navigator.credentials.get({ publicKey: decodeOptions(publicKey) });

    const res = await app.pb.send(base + "/step-up-finish", {
        method: "POST",
        body: {
            sessionToken: begin.sessionToken,
            id: cred.id,
            rawId: bufToB64u(cred.rawId),
            type: cred.type,
            clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
            response: {
                authenticatorData: bufToB64u(cred.response.authenticatorData),
                clientDataJSON: bufToB64u(cred.response.clientDataJSON),
                signature: bufToB64u(cred.response.signature),
                userHandle: cred.response.userHandle ? bufToB64u(cred.response.userHandle) : null,
            },
        },
    });

    if (res?.token) {
        app.pb.authStore.save(res.token, app.pb.authStore.record);
    }
}

/**
 * Runs fn. When the server answers that a recent passkey is required, runs the step-up and
 * tries once more. Other errors pass through.
 */
export async function withStepUp(fn) {
    try {
        return await fn();
    } catch (err) {
        if (!isStepUpRequired(err)) {
            throw err;
        }
    }
    await stepUp();
    return await fn();
}

// ---- page frame ----

export function gdprSidebar() {
    return app.components.pageSidebar(
        { pbEvent: "gdprSidebar", className: "settings-sidebar gdpr-sidebar" },
        t.nav(
            { className: "sidebar-content scrollable" },
            t.details(
                { className: "nav-group", open: true },
                t.summary({ tabIndex: -1, onfocusout: () => false, onclick: () => false, onkeyup: () => false }, "Data protection"),
                GDPR_LINKS.map((link) =>
                    t.a(
                        {
                            href: link.href,
                            className: () => `nav-item ${app.utils.isActivePath(link.href, false) ? "active" : ""}`,
                        },
                        t.i({ className: link.icon, ariaHidden: true }),
                        t.span({ className: "txt" }, link.label),
                    )
                ),
            ),
        ),
    );
}

/** The error handler of a page: `fail(err)` fills state.error. */
export function failer(state) {
    return (err) => {
        if (err?.isAbort) {
            return;
        }
        if (err?.status == 401) {
            return app.checkApiError(err);
        }
        state.error = message(err);
    };
}

export function flag(text, cls = "") {
    return t.span({ className: `label sm ${cls}` }, text);
}

export function iconButton(icon, title, onclick, extra = "") {
    return t.button(
        { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
        t.i({ className: icon, ariaHidden: true }),
    );
}

/**
 * The frame of a page: sidebar, header with the title and actions, alerts, then the body.
 * `state` needs `error` and `notice`; `idPrefix` names the alert ids.
 */
export function gdprPage({ pbEvent, title, state, idPrefix, actions, onmount }, ...body) {
    app.store.title = title;
    return t.div(
        { pbEvent, className: `page ${pbEvent}`, onmount },
        gdprSidebar(),
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav(
                    { className: "breadcrumbs" },
                    t.div({ className: "breadcrumb-item" }, "Data protection"),
                    t.div({ className: "breadcrumb-item" }, title),
                ),
                t.div({ className: "inline-flex gap-sm" }, actions || null),
            ),
            t.div(
                { className: "page-table-wrapper" },
                () => state.error ? t.div({ className: "alert danger m-b-10", id: `${idPrefix}_error`, role: "alert" }, state.error) : null,
                () => state.notice ? t.div({ className: "alert success m-b-10", id: `${idPrefix}_notice`, role: "status" }, state.notice) : null,
                ...body,
            ),
        ),
    );
}

export function cell(v) {
    if (v === undefined || v === null || v === "") return "";
    return typeof v == "object" ? JSON.stringify(v) : String(v);
}
