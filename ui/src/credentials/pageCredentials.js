// Fork-local (Claviger #81, 81-4): safeguarding credentials of an organization, for superusers.
// Everything goes through /api/orgs/{id}/credentials, /credential-types and /access-status, so the
// rules of the model (nobody records or verifies their own, a second verifier who is neither the
// recorder nor the holder) apply here too and the server answers when they are broken.

const STATES = ["all", "valid", "expiring", "pending", "lapsed", "revoked"];
const STANDINGS = ["cleared", "expiring", "pending", "lapsed", "revoked", "missing"];
const EXPIRING_DAYS = 60;

// the message of a failed request, in words the operator can act on
export function credentialError(err) {
    const status = err?.status || err?.response?.status;
    const msg = err?.response?.message || err?.message || "Request failed.";
    if (status == 404) {
        return `Credentials are not available for this organization or server (404). ${msg}`;
    }
    if (status == 403) {
        return `Not allowed. ${msg}`;
    }
    return msg;
}

// the dates of a credential row in the list: a day, no time
export function day(v) {
    return String(v || "").slice(0, 10);
}

// is a listed credential valid but within the expiring window?
export function isExpiring(c, now = Date.now(), days = EXPIRING_DAYS) {
    if (c.state != "valid") {
        return false;
    }
    const exp = Date.parse(String(c.expires || "").replace(" ", "T"));
    return !isNaN(exp) && exp - now <= days * 86400000;
}

export function pageCredentials(route) {
    app.store.title = "Credentials";

    const state = store({
        orgs: [],
        orgId: route.query.org?.[0] || "",
        stateFilter: "all",
        typeFilter: "",
        members: [],
        types: [],
        items: [],
        summary: [],
        snippet: null,
        error: "",
        notice: "",
        isLoading: false,
    });

    const failed = (err) => {
        if (err?.isAbort) {
            return;
        }
        if (err?.status == 401) {
            return app.checkApiError(err);
        }
        state.error = credentialError(err);
    };

    const base = () => `/api/orgs/${state.orgId}`;
    const typeName = (key) => state.types.find((x) => x.key == key)?.name || key;
    const person = (id) => {
        const m = state.members.find((x) => x.user == id);
        return m ? m.name || m.email || id : id;
    };
    const visible = () =>
        state.items.filter((c) =>
            (!state.typeFilter || c.type == state.typeFilter) &&
            (state.stateFilter == "all" ||
                (state.stateFilter == "expiring" ? isExpiring(c) : c.state == state.stateFilter))
        );

    async function send(path, opts) {
        try {
            return await app.pb.send(path, opts);
        } catch (err) {
            // the modal shows err.response.message
            err.response = { ...(err.response || {}), message: credentialError(err) };
            throw err;
        }
    }

    async function loadOrgs() {
        try {
            const res = await app.pb.send("/api/orgs", { query: { perPage: 200 } });
            state.orgs = res.items;
            if (!state.orgs.find((o) => o.id == state.orgId)) {
                state.orgId = state.orgs[0]?.id || "";
            }
        } catch (err) {
            failed(err);
        }
    }

    async function loadAll() {
        state.snippet = null;
        if (!state.orgId) {
            state.items = state.types = state.summary = state.members = [];
            return;
        }
        state.isLoading = true;
        state.error = "";
        try {
            const [types, creds, status, members] = await Promise.all([
                app.pb.send(`${base()}/credential-types`),
                app.pb.send(`${base()}/credentials`, { query: { perPage: 500 } }),
                app.pb.send(`${base()}/access-status`, { query: { limit: 1 } }),
                app.pb.send(`${base()}/members`, { query: { perPage: 200 } }),
            ]);
            state.types = types.items;
            state.items = creds.items;
            state.summary = status.types;
            state.members = members.items.filter((m) => m.status == "active");
        } catch (err) {
            state.items = state.types = state.summary = [];
            failed(err);
        }
        state.isLoading = false;
    }

    // ---- credentials ----------------------------------------------------------
    function addCredential() {
        const active = state.types.filter((x) => x.status == "active");
        app.modals.openOrgForm({
            title: "Record a credential",
            submitLabel: "Record",
            fields: [
                {
                    id: "user",
                    label: "Holder",
                    type: "select",
                    options: state.members.map((m) => ({ value: m.user, label: m.name || m.email || m.user })),
                    value: state.members[0]?.user || "",
                },
                {
                    id: "type",
                    label: "Credential type",
                    type: "select",
                    options: active.map((x) => ({ value: x.key, label: x.name })),
                    value: active[0]?.key || "",
                },
                { id: "issuedAt", label: "Issued on (YYYY-MM-DD, optional)", placeholder: "2026-01-31" },
                { id: "expires", label: "Expires on (YYYY-MM-DD)", hint: "Empty uses the validity of the type. A date without a time means the end of that day (UTC).", placeholder: "2027-01-31" },
                { id: "issuer", label: "Issued by (optional)", hint: "For example the training provider." },
            ],
            onsubmit: (v) => {
                const body = { user: v.user, type: v.type };
                for (const k of ["issuedAt", "expires", "issuer"]) {
                    if (v[k].trim()) {
                        body[k] = v[k].trim();
                    }
                }
                return send(`${base()}/credentials`, { method: "POST", body });
            },
            onsave: loadAll,
        });
    }

    function renew(c) {
        app.modals.openOrgForm({
            title: `Renew ${typeName(c.type)} for ${person(c.user)}`,
            fields: [
                { id: "issuedAt", label: "Issued on (YYYY-MM-DD)", value: day(c.issuedAt) },
                { id: "expires", label: "Expires on (YYYY-MM-DD)", value: day(c.expires), hint: "A type with a second verifier needs verifying again after a change." },
                { id: "issuer", label: "Issued by", value: c.issuer || "" },
            ],
            onsubmit: (v) =>
                send(`${base()}/credentials/${c.id}`, {
                    method: "PATCH",
                    body: { issuedAt: v.issuedAt.trim(), expires: v.expires.trim(), issuer: v.issuer },
                }),
            onsave: loadAll,
        });
    }

    async function verify(c) {
        state.error = state.notice = "";
        try {
            await app.pb.send(`${base()}/credentials/${c.id}/verify`, { method: "POST" });
            state.notice = `${typeName(c.type)} for ${person(c.user)} was verified.`;
            await loadAll();
        } catch (err) {
            // typically 403: the verifier may be neither the recorder nor the holder
            failed(err);
        }
    }

    function revoke(c) {
        app.modals.openOrgForm({
            title: `Revoke ${typeName(c.type)} for ${person(c.user)}`,
            submitLabel: "Revoke",
            fields: [{ id: "reason", label: "Reason", hint: "Kept for coordinators. The holder is not shown it." }],
            onsubmit: (v) => send(`${base()}/credentials/${c.id}/revoke`, { method: "POST", body: { reason: v.reason } }),
            onsave: loadAll,
        });
    }

    // ---- types ----------------------------------------------------------------
    function typeFields(x) {
        return [
            ...(x ? [] : [{ id: "key", label: "Key", required: true, hint: "a-z, 0-9 and _, 2 to 40 characters. Cannot be changed.", placeholder: "safe_environment" }]),
            { id: "name", label: "Name", value: x?.name || "", required: true },
            { id: "validMonths", label: "Valid for (months, 0 = set each expiry by hand)", value: String(x?.validMonths ?? 12) },
            { id: "requiresVerifier", label: "Needs a second verifier", type: "checkbox", value: !!x?.requiresVerifier },
        ];
    }

    function typeBody(v, x) {
        const months = parseInt(v.validMonths, 10);
        return {
            ...(x ? {} : { key: v.key.trim() }),
            name: v.name,
            validMonths: isNaN(months) ? 0 : months,
            requiresVerifier: v.requiresVerifier,
        };
    }

    function addType() {
        app.modals.openOrgForm({
            title: "New credential type",
            submitLabel: "Create",
            fields: typeFields(null),
            onsubmit: (v) => send(`${base()}/credential-types`, { method: "POST", body: typeBody(v, null) }),
            onsave: loadAll,
        });
    }

    function editType(x) {
        app.modals.openOrgForm({
            title: `Edit ${x.name}`,
            fields: typeFields(x),
            onsubmit: (v) => send(`${base()}/credential-types/${x.id}`, { method: "PATCH", body: typeBody(v, x) }),
            onsave: loadAll,
        });
    }

    async function setTypeStatus(x, status) {
        state.error = "";
        try {
            await app.pb.send(`${base()}/credential-types/${x.id}`, { method: "PATCH", body: { status } });
            await loadAll();
        } catch (err) {
            failed(err);
        }
    }

    function deleteType(x) {
        app.modals.confirm(`Delete the type ${x.name}? A type with credentials cannot be deleted: retire it instead.`, async () => {
            try {
                await app.pb.send(`${base()}/credential-types/${x.id}`, { method: "DELETE" });
                await loadAll();
            } catch (err) {
                failed(err);
            }
        });
    }

    async function showSnippet(x) {
        state.error = "";
        try {
            const res = await app.pb.send("/api/orgs/rule-templates", {
                query: { field: "org", credentialType: x.key, credentialScope: "ancestors" },
            });
            state.snippet = { key: x.key, set: res.credentialGated };
        } catch (err) {
            state.snippet = null;
            failed(err);
        }
    }

    // ---- views ----------------------------------------------------------------
    function btn(icon, label, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary ${extra}`, title: label, "aria-label": label, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    function flag(text, cls = "") {
        return t.span({ className: `label sm ${cls}` }, text);
    }

    const stateClass = { valid: "success", pending: "warning", lapsed: "danger", revoked: "danger" };

    function summaryView() {
        return t.div(
            { id: "credential_summary", className: "m-b-20" },
            t.h6(null, "Who is cleared"),
            () => {
                if (!state.summary.length) {
                    return t.div({ className: "txt-hint txt-sm" }, state.isLoading ? "Loading..." : "No credential types apply to this organization yet.");
                }
                return t.table(
                    { className: "credential-summary-table" },
                    t.thead(null, t.tr(null, t.th(null, "Type"), STANDINGS.map((s) => t.th({ className: "txt-right" }, s)))),
                    t.tbody(
                        null,
                        state.summary.map((x) =>
                            t.tr(
                                { "html-data-summary-type": x.key },
                                t.td(null, t.span({ className: "txt-bold" }, x.name)),
                                STANDINGS.map((s) => t.td({ className: "txt-right", "html-data-standing": s }, String(x.counts[s] ?? 0))),
                            )
                        ),
                    ),
                );
            },
        );
    }

    function credentialsView() {
        return t.div(
            { id: "credential_list" },
            t.div(
                { className: "grid m-b-10" },
                t.div(
                    { className: "col-lg-3" },
                    t.div(
                        { className: "field" },
                        t.select(
                            { id: "credentials_state", onchange: (e) => state.stateFilter = e.target.value },
                            STATES.map((s) => t.option({ value: s, selected: () => state.stateFilter == s }, s)),
                        ),
                        t.label({ htmlFor: "credentials_state" }, "State"),
                    ),
                ),
                t.div(
                    { className: "col-lg-3" },
                    t.div(
                        { className: "field" },
                        t.select(
                            { id: "credentials_type", onchange: (e) => state.typeFilter = e.target.value },
                            () => [t.option({ value: "" }, "All types"), ...state.types.map((x) => t.option({ value: x.key, selected: () => state.typeFilter == x.key }, x.name))],
                        ),
                        t.label({ htmlFor: "credentials_type" }, "Type"),
                    ),
                ),
            ),
            t.table(
                { className: "credentials-table" },
                t.thead(
                    null,
                    t.tr(null, t.th(null, "Holder"), t.th(null, "Type"), t.th(null, "State"), t.th(null, "Issued"), t.th(null, "Expires"), t.th(null, "Issued by"), t.th({ className: "col-meta" }, "Actions")),
                ),
                t.tbody(null, () => {
                    const rows = visible();
                    if (!rows.length) {
                        return t.tr(null, t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No credentials.")));
                    }
                    return rows.map((c) =>
                        t.tr(
                            { "html-data-credential-id": c.id },
                            t.td(null, t.span({ className: "txt-bold" }, person(c.user))),
                            t.td(null, typeName(c.type)),
                            t.td(
                                null,
                                t.div(
                                    { className: "inline-flex gap-xs" },
                                    flag(c.state, stateClass[c.state] || ""),
                                    isExpiring(c) ? flag("expiring", "warning") : null,
                                ),
                                c.state == "revoked" && c.revokedReason ? t.div({ className: "txt-hint txt-sm" }, c.revokedReason) : null,
                            ),
                            t.td(null, day(c.issuedAt)),
                            t.td(null, day(c.expires)),
                            t.td(null, c.issuer || t.span({ className: "txt-hint" }, "-")),
                            t.td(
                                { className: "col-meta" },
                                t.div(
                                    { className: "inline-flex gap-xs" },
                                    c.state == "pending" ? btn("ri-check-double-line", `Verify ${typeName(c.type)} for ${person(c.user)}`, () => verify(c)) : null,
                                    c.state != "revoked" ? btn("ri-refresh-line", `Renew ${typeName(c.type)} for ${person(c.user)}`, () => renew(c)) : null,
                                    c.state != "revoked" ? btn("ri-forbid-2-line", `Revoke ${typeName(c.type)} for ${person(c.user)}`, () => revoke(c), "danger") : null,
                                ),
                            ),
                        )
                    );
                }),
            ),
            () => state.items.length >= 500 ? t.div({ className: "txt-hint txt-sm m-t-5" }, "Showing the first 500 credentials. Filter by type or state.") : null,
            t.div(
                { className: "txt-hint txt-sm m-t-5" },
                "A type with a second verifier starts as pending. The verifier must be a coordinator who is neither the recorder nor the holder.",
            ),
        );
    }

    function typesView() {
        return t.div(
            { id: "credential_types", className: "m-t-30" },
            t.div(
                { className: "flex m-b-10" },
                t.h6({ className: "m-r-auto" }, "Credential types"),
                t.button({ type: "button", className: "btn sm secondary", disabled: () => !state.orgId, onclick: addType }, t.i({ className: "ri-add-line", ariaHidden: true }), t.span({ className: "txt" }, "New type")),
            ),
            t.table(
                { className: "credential-types-table" },
                t.thead(null, t.tr(null, t.th(null, "Name"), t.th(null, "Key"), t.th(null, "Valid for"), t.th(null, "Second verifier"), t.th(null, "Status"), t.th({ className: "col-meta" }, "Actions"))),
                t.tbody(null, () => {
                    if (!state.types.length) {
                        return t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No types.")));
                    }
                    return state.types.map((x) => {
                        // a type defined by an ancestor org is read-only here
                        const own = x.org == state.orgId;
                        return t.tr(
                            { "html-data-type-key": x.key },
                            t.td(null, t.span({ className: "txt-bold" }, x.name), own ? null : t.span({ className: "txt-hint" }, " (inherited)")),
                            t.td(null, t.code(null, x.key)),
                            t.td(null, x.validMonths ? `${x.validMonths} months` : "set per credential"),
                            t.td(null, x.requiresVerifier ? "yes" : "no"),
                            t.td(null, flag(x.status, x.status == "active" ? "success" : "")),
                            t.td(
                                { className: "col-meta" },
                                t.div(
                                    { className: "inline-flex gap-xs" },
                                    btn("ri-code-s-slash-line", `Rule snippet for ${x.name}`, () => showSnippet(x)),
                                    own ? btn("ri-pencil-line", `Edit ${x.name}`, () => editType(x)) : null,
                                    own ? btn(x.status == "active" ? "ri-archive-line" : "ri-inbox-unarchive-line", `${x.status == "active" ? "Retire" : "Reactivate"} ${x.name}`, () => setTypeStatus(x, x.status == "active" ? "retired" : "active")) : null,
                                    own ? btn("ri-delete-bin-7-line", `Delete ${x.name}`, () => deleteType(x), "danger") : null,
                                ),
                            ),
                        );
                    });
                }),
            ),
            () => snippetView(),
        );
    }

    function snippetView() {
        const sn = state.snippet;
        if (!sn) {
            return null;
        }
        const rules = [["List", sn.set.listRule], ["View", sn.set.viewRule], ["Create", sn.set.createRule], ["Update", sn.set.updateRule], ["Delete", sn.set.deleteRule]];
        return t.div(
            { id: "credential_snippet", className: "m-t-10" },
            t.p(
                { className: "txt-hint txt-sm" },
                `Rules for a collection with an "org" relation whose members need a current ${sn.key} credential issued by the record's org or any org above it. Paste them into the collection's API rules. A lapse closes access at once.`,
            ),
            rules.map(([label, rule]) =>
                t.div({ className: "m-b-5" }, t.div({ className: "txt-bold txt-sm" }, label), t.code({ className: "credential-rule" }, rule ?? "(superusers only)"))
            ),
        );
    }

    const watchers = [];

    return t.div(
        {
            pbEvent: "pageCredentials",
            className: "page page-credentials",
            onmount() {
                loadOrgs().then(loadAll);
                let lastOrg = state.orgId;
                watchers.push(
                    watch(() => state.orgId, () => {
                        app.utils.replaceHashQueryParams({ org: state.orgId });
                        if (state.orgId == lastOrg) {
                            return;
                        }
                        lastOrg = state.orgId;
                        state.notice = "";
                        state.typeFilter = "";
                        loadAll();
                    }),
                );
            },
            onunmount() {
                watchers.forEach((w) => w?.unwatch());
            },
        },
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav({ className: "breadcrumbs" }, t.div(null, "Credentials")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({ onclick: loadAll }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", disabled: () => !state.orgId || !state.types.length, onclick: addCredential },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Record credential"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                t.div(
                    { className: "grid m-b-10" },
                    t.div(
                        { className: "col-lg-5" },
                        t.div(
                            { className: "field" },
                            t.select(
                                { id: "credentials_org", onchange: (e) => state.orgId = e.target.value },
                                () => state.orgs.length
                                    ? state.orgs.map((o) => t.option({ value: o.id, selected: () => state.orgId == o.id }, o.name))
                                    : t.option({ value: "" }, "(no organizations)"),
                            ),
                            t.label({ htmlFor: "credentials_org" }, "Organization"),
                        ),
                    ),
                ),
                () => state.error ? t.div({ className: "alert danger m-b-10", id: "credentials_error", role: "alert" }, state.error) : null,
                () => state.notice ? t.div({ className: "alert success m-b-10", id: "credentials_notice", role: "status" }, state.notice) : null,
                summaryView(),
                credentialsView(),
                typesView(),
            ),
        ),
    );
}
