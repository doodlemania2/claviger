// Fork-local (Claviger #88, 88-2): consent purposes and a person's consent
// history, for superusers. Everything goes through /api/consent-purposes and
// /api/consents/history, so the rules of the model (a changed text raises the
// version, a used purpose is archived and not deleted) apply here too.

const BASES = ["consent", "legitimate_interest", "legal_obligation", "contract", "vital_interest", "public_task"];

function message(err) {
    return err?.response?.message || err?.message || "Request failed.";
}

export function pageConsents() {
    app.store.title = "Consents";

    const state = store({
        purposes: [],
        error: "",
        notice: "",
        isLoading: false,
        subject: "",
        history: null,
        isLoadingHistory: false,
    });

    const failed = (err) => {
        if (err?.isAbort) {
            return;
        }
        if (err?.status == 401) {
            return app.checkApiError(err);
        }
        state.error = message(err);
    };

    async function load() {
        state.isLoading = true;
        try {
            const res = await app.pb.send("/api/consent-purposes");
            state.purposes = res.items;
            state.error = "";
        } catch (err) {
            state.purposes = [];
            failed(err);
        }
        state.isLoading = false;
    }

    async function send(path, opts) {
        try {
            return await app.pb.send(path, opts);
        } catch (err) {
            // the modal shows err.response.message
            err.response = { ...(err.response || {}), message: message(err) };
            throw err;
        }
    }

    function fields(p) {
        return [
            { id: "key", label: "Key (a-z, 0-9, _ and -; cannot change)", value: p?.key || "", required: true, disabled: !!p },
            { id: "name", label: "Name", value: p?.name || "", required: true },
            { id: "text", label: "Text shown to the person (a change raises the version)", type: "textarea", value: p?.text || "", required: true },
            { id: "lawfulBasis", label: "Lawful basis", type: "select", options: BASES, value: p?.lawfulBasis || "consent" },
            { id: "minAge", label: "Minimum age (0 for none)", value: String(p?.minAge ?? 0) },
            { id: "requiresGuardian", label: "A guardian must give it for a minor", type: "checkbox", value: !!p?.requiresGuardian },
            { id: "marketing", label: "Direct marketing (never for a minor)", type: "checkbox", value: !!p?.marketing },
            ...(p ? [{ id: "status", label: "Status", type: "select", options: ["active", "archived"], value: p.status }] : []),
        ];
    }

    function body(v, p) {
        const out = {
            name: v.name,
            text: v.text,
            lawfulBasis: v.lawfulBasis,
            minAge: parseInt(v.minAge || "0", 10),
            requiresGuardian: v.requiresGuardian,
            marketing: v.marketing,
        };
        if (!p) {
            out.key = v.key;
        } else {
            out.status = v.status;
        }
        return out;
    }

    function newPurpose() {
        app.modals.openOrgForm({
            title: "New consent purpose",
            submitLabel: "Create",
            fields: fields(null),
            onsubmit: (v) => send("/api/consent-purposes", { method: "POST", body: body(v, null) }),
            onsave: load,
        });
    }

    function editPurpose(p) {
        app.modals.openOrgForm({
            title: `Edit ${p.name}`,
            fields: fields(p),
            onsubmit: async (v) => {
                const res = await send(`/api/consent-purposes/${p.id}`, { method: "PATCH", body: body(v, p) });
                state.notice = res.version != p.version
                    ? `${p.name} is now version ${res.version}. Existing consents show as given on version ${p.version}.`
                    : "";
            },
            onsave: load,
        });
    }

    function deletePurpose(p) {
        app.modals.confirm(`Delete ${p.name}? A purpose with consent records cannot be deleted: archive it instead.`, async () => {
            try {
                await app.pb.send(`/api/consent-purposes/${p.id}`, { method: "DELETE" });
                state.error = "";
                await load();
            } catch (err) {
                failed(err);
            }
        });
    }

    async function loadHistory() {
        const subject = state.subject.trim();
        if (!subject) {
            state.history = null;
            return;
        }
        state.isLoadingHistory = true;
        try {
            state.history = await app.pb.send("/api/consents/history", { query: { subject } });
            state.error = "";
        } catch (err) {
            state.history = null;
            failed(err);
        }
        state.isLoadingHistory = false;
    }

    function btn(icon, title, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    function flag(text, cls = "") {
        return t.span({ className: `label sm ${cls}` }, text);
    }

    function historyView() {
        const h = state.history;
        if (!h) {
            return t.div({ className: "txt-hint txt-sm" }, "Enter a household member id or a user id to see what they agreed to.");
        }
        return t.div(
            { id: "consent_history" },
            t.h6({ className: "m-b-10" }, "Consents"),
            t.table(
                { className: "consent-states-table" },
                t.thead(null, t.tr(null, t.th(null, "Purpose"), t.th(null, "Status"), t.th(null, "Version"), t.th(null, "Given"), t.th(null, "Withdrawn"), t.th(null, "Method"), t.th(null, "Evidence"))),
                t.tbody(
                    null,
                    h.consents.length
                        ? h.consents.map((c) =>
                            t.tr(
                                null,
                                t.td(null, c.purpose),
                                t.td(null, flag(c.status, c.status == "granted" ? "success" : "warning"), c.outdated ? flag("older version") : null),
                                t.td(null, `${c.policyVersion}${c.currentVersion ? ` of ${c.currentVersion}` : ""}`),
                                t.td(null, c.grantedAt || ""),
                                t.td(null, c.withdrawnAt || ""),
                                t.td(null, c.method),
                                t.td(null, c.evidenceRef || ""),
                            )
                        )
                        : t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No consents."))),
                ),
            ),
            t.h6({ className: "m-t-20 m-b-10" }, "History (append-only)"),
            t.table(
                { className: "consent-events-table" },
                t.thead(null, t.tr(null, t.th(null, "When"), t.th(null, "Purpose"), t.th(null, "Action"), t.th(null, "Version"), t.th(null, "Method"), t.th(null, "Given by"), t.th(null, "Done by"), t.th(null, "Evidence"))),
                t.tbody(
                    null,
                    h.events.length
                        ? h.events.map((e) =>
                            t.tr(
                                { title: e.text },
                                t.td(null, e.created),
                                t.td(null, e.purpose),
                                t.td(null, e.action),
                                t.td(null, String(e.policyVersion)),
                                t.td(null, e.method),
                                t.td(null, e.givenBy ? t.code(null, e.givenBy) : ""),
                                t.td(null, e.actor ? t.code(null, e.actor) : ""),
                                t.td(null, e.evidenceRef || ""),
                            )
                        )
                        : t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No events."))),
                ),
            ),
        );
    }

    return t.div(
        {
            pbEvent: "pageConsents",
            className: "page page-consents",
            onmount() {
                load();
            },
        },
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav({ className: "breadcrumbs" }, t.div(null, "Consents")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({ onclick: load }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", onclick: newPurpose },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New purpose"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                () => state.error ? t.div({ className: "alert danger m-b-10", id: "consents_error", role: "alert" }, state.error) : null,
                () => state.notice ? t.div({ className: "alert success m-b-10", id: "consents_notice", role: "status" }, state.notice) : null,
                t.table(
                    { className: "consent-purposes-table" },
                    t.thead(null, t.tr(null, t.th(null, "Key"), t.th(null, "Name"), t.th(null, "Version"), t.th(null, "Basis"), t.th(null, "Flags"), t.th({ className: "col-meta" }, "Actions"))),
                    t.tbody(null, () => {
                        if (!state.purposes.length) {
                            return t.tr(
                                null,
                                t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No purposes.")),
                            );
                        }
                        return state.purposes.map((p) =>
                            t.tr(
                                { "html-data-purpose-key": p.key },
                                t.td(null, t.code(null, p.key)),
                                t.td(null, t.span({ className: "txt-bold" }, p.name)),
                                t.td(null, String(p.version)),
                                t.td(null, p.lawfulBasis),
                                t.td(
                                    null,
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        p.status != "active" ? flag(p.status, "warning") : null,
                                        p.marketing ? flag("marketing") : null,
                                        p.requiresGuardian ? flag("guardian") : null,
                                        p.minAge ? flag("age " + p.minAge + "+") : null,
                                        p.org ? flag("org") : null,
                                    ),
                                ),
                                t.td(
                                    { className: "col-meta" },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        btn("ri-pencil-line", "Edit " + p.name, () => editPurpose(p)),
                                        btn("ri-delete-bin-7-line", "Delete " + p.name, () => deletePurpose(p), "danger"),
                                    ),
                                ),
                            )
                        );
                    }),
                ),
                t.div(
                    { className: "m-t-30" },
                    t.h6({ className: "m-b-10" }, "A person's consent history"),
                    t.form(
                        {
                            className: "grid m-b-10",
                            onsubmit: (e) => {
                                e.preventDefault();
                                loadHistory();
                            },
                        },
                        t.div(
                            { className: "col-lg-5" },
                            t.div(
                                { className: "field" },
                                t.input({ id: "consent_subject", type: "text", value: () => state.subject, oninput: (e) => state.subject = e.target.value }),
                                t.label({ htmlFor: "consent_subject" }, "Household member id or user id"),
                            ),
                        ),
                        t.div(
                            { className: "col-lg-3" },
                            t.button(
                                { type: "submit", className: () => `btn secondary ${state.isLoadingHistory ? "loading" : ""}` },
                                t.span({ className: "txt" }, "Show history"),
                            ),
                        ),
                    ),
                    () => historyView(),
                ),
            ),
        ),
    );
}
