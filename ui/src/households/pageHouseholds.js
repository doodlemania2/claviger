// Fork-local (Claviger #80, 80-5): households of an organization, for superusers.
// Everything goes through /api/households, so the guards of the model (a minor
// has no login, a household with a minor keeps an adult guardian) apply here too.
// Merge, split and duplicates are written against the contract of #80:
//   POST /api/households/{id}/merge  {from}                 -> household with members
//   POST /api/households/{id}/split  {members, name}        -> {from, to}
//   GET  /api/households/duplicates?org=ID                  -> candidates
// A server without those routes answers 404, which the page says in words.

const ROLES = ["head", "spouse", "adult", "child", "other"];
const MEMBER_STATUSES = ["active", "left", "deceased"];
const HOUSEHOLD_STATUSES = ["active", "archived"];

// the message of a failed request, in words the operator can act on
export function householdError(err, routeName) {
    const status = err?.status || err?.response?.status;
    const msg = err?.response?.message || err?.message || "Request failed.";
    if (status == 409 && /not encrypted/i.test(msg)) {
        return "The encrypted fields are not set up: configure a key-encryption key (CLAVIGER_KEK) and restart the server. " + msg;
    }
    if (status == 404 && routeName) {
        return `${routeName} is not available on this server (404). It needs a build that has this route. ${msg}`;
    }
    return msg;
}

// duplicate candidates: the shape of the answer is read leniently (see docs/households.md)
export function normalizeCandidates(res) {
    const list = Array.isArray(res) ? res : res?.items || res?.candidates || res?.duplicates || [];
    const side = (c, k, i) => {
        const x = c[k] || c.members?.[i] || c.households?.[i] || {};
        return {
            member: x.member || x.memberId || (x.household ? x.id : "") || c[k + "Member"] || "",
            name: x.name || x.memberName || c[k + "Name"] || "",
            household: x.household || x.householdId || c[k + "Household"] || (x.members ? x.id : "") || "",
            householdName: x.householdName || c[k + "HouseholdName"] || "",
        };
    };
    return list.map((c) => ({
        a: side(c, "a", 0),
        b: side(c, "b", 1),
        reason: c.reason || "",
        raw: c,
    }));
}

export function pageHouseholds(route) {
    app.store.title = "Households";

    const state = store({
        orgs: [],
        orgId: route.query.org?.[0] || "",
        statusFilter: "all",
        households: [],
        activeId: route.query.household?.[0] || "",
        detail: null,
        duplicates: null,
        error: "",
        notice: "",
        isLoading: false,
        isLoadingDetail: false,
        isLoadingDuplicates: false,
    });

    const org = () => state.orgs.find((o) => o.id == state.orgId);
    const visible = () => state.households.filter((h) => state.statusFilter == "all" || h.status == state.statusFilter);
    const failed = (err, routeName) => {
        if (err?.isAbort) {
            return;
        }
        if (err?.status == 401) {
            return app.checkApiError(err);
        }
        state.error = householdError(err, routeName);
    };

    async function loadOrgs() {
        try {
            const res = await app.pb.send("/api/orgs", { query: { perPage: 200 } });
            state.orgs = res.items;
            if (!org()) {
                state.orgId = state.orgs[0]?.id || "";
            }
        } catch (err) {
            failed(err);
        }
    }

    async function loadHouseholds() {
        state.duplicates = null;
        if (!state.orgId) {
            state.households = [];
            return;
        }
        state.isLoading = true;
        try {
            const res = await app.pb.send("/api/households", { query: { org: state.orgId, perPage: 200 } });
            state.households = res.items;
            state.error = "";
            if (!state.households.find((h) => h.id == state.activeId)) {
                state.activeId = "";
                state.detail = null;
            }
        } catch (err) {
            state.households = [];
            failed(err, "The households list");
        }
        state.isLoading = false;
    }

    async function loadDetail() {
        if (!state.activeId) {
            state.detail = null;
            return;
        }
        state.isLoadingDetail = true;
        try {
            state.detail = await app.pb.send(`/api/households/${state.activeId}`);
        } catch (err) {
            state.detail = null;
            failed(err, "The household");
        }
        state.isLoadingDetail = false;
    }

    async function reload() {
        await loadHouseholds();
        await loadDetail();
    }

    // ---- household ----------------------------------------------------------
    function newHousehold() {
        app.modals.openOrgForm({
            title: "New household",
            submitLabel: "Create",
            fields: [
                { id: "name", label: "Household name", required: true },
                { id: "first", label: "First person (optional)", hint: "Added as head and guardian." },
                { id: "street", label: "Street" },
                { id: "city", label: "City" },
                { id: "homePhone", label: "Home phone" },
            ],
            onsubmit: async (v) => {
                const body = { org: state.orgId, name: v.name };
                if (v.first.trim()) {
                    body.first = { name: v.first.trim(), role: "head", guardian: true };
                }
                if (v.street || v.city) {
                    body.address = { street: v.street, city: v.city };
                }
                if (v.homePhone) {
                    body.homePhone = v.homePhone;
                }
                const created = await sendOrExplain("/api/households", { method: "POST", body });
                state.activeId = created.id;
            },
            onsave: reload,
        });
    }

    function editHousehold() {
        const h = state.detail;
        const addr = h.address && typeof h.address == "object" ? h.address : {};
        app.modals.openOrgForm({
            title: "Edit household",
            fields: [
                { id: "name", label: "Name", value: h.name, required: true },
                { id: "street", label: "Street", value: addr.street || "" },
                { id: "city", label: "City", value: addr.city || "" },
                { id: "region", label: "State or region", value: addr.region || "" },
                { id: "postal", label: "Postal code", value: addr.postal || "" },
                { id: "homePhone", label: "Home phone", value: h.homePhone || "" },
                { id: "status", label: "Status", type: "select", options: HOUSEHOLD_STATUSES, value: h.status },
            ],
            onsubmit: (v) =>
                sendOrExplain(`/api/households/${h.id}`, {
                    method: "PATCH",
                    body: {
                        name: v.name,
                        // keep the keys this form does not know about
                        address: { ...addr, street: v.street, city: v.city, region: v.region, postal: v.postal },
                        homePhone: v.homePhone,
                        status: v.status,
                    },
                }),
            onsave: reload,
        });
    }

    function setHouseholdStatus(status) {
        const h = state.detail;
        app.modals.confirm(
            status == "archived" ? `Archive ${h.name}? Its people stay, but nobody can add to it.` : `Restore ${h.name}?`,
            async () => {
                try {
                    await app.pb.send(`/api/households/${h.id}`, { method: "PATCH", body: { status } });
                    await reload();
                } catch (err) {
                    failed(err);
                }
            },
        );
    }

    // ---- people -------------------------------------------------------------
    function memberFields(m) {
        return [
            { id: "name", label: "Name", value: m?.name || "", required: true },
            { id: "role", label: "Role", type: "select", options: ROLES, value: m?.role || "adult" },
            { id: "guardian", label: "Guardian", type: "checkbox", value: !!m?.guardian },
            { id: "isMinor", label: "Minor (follows the birth date when there is one)", type: "checkbox", value: !!m?.isMinor },
            { id: "birthDate", label: "Birth date (YYYY-MM-DD)", value: (m?.birthDate || "").slice(0, 10), placeholder: "2014-05-17" },
            { id: "allergies", label: "Allergies", value: m?.allergies || "" },
            { id: "medical", label: "Medical notes", value: m?.medical || "" },
            { id: "user", label: "Login (user id)", value: m?.user || "", hint: "A minor never gets a login. Empty means none." },
            ...(m ? [{ id: "status", label: "Status", type: "select", options: MEMBER_STATUSES, value: m.status }] : []),
        ];
    }

    function memberBody(v, m) {
        const body = {
            name: v.name,
            role: v.role,
            guardian: v.guardian,
            isMinor: v.isMinor,
            birthDate: v.birthDate.trim(),
            allergies: v.allergies,
            medical: v.medical,
        };
        if (!m || v.user.trim() != (m.user || "")) {
            body.user = v.user.trim();
        }
        if (m) {
            body.status = v.status;
        }
        return body;
    }

    function addMember() {
        const h = state.detail;
        app.modals.openOrgForm({
            title: "Add a person",
            submitLabel: "Add",
            fields: memberFields(null),
            onsubmit: (v) => sendOrExplain(`/api/households/${h.id}/members`, { method: "POST", body: memberBody(v, null) }),
            onsave: loadDetail,
        });
    }

    function editMember(m) {
        const h = state.detail;
        app.modals.openOrgForm({
            title: `Edit ${m.name}`,
            fields: memberFields(m),
            onsubmit: (v) => sendOrExplain(`/api/households/${h.id}/members/${m.id}`, { method: "PATCH", body: memberBody(v, m) }),
            onsave: loadDetail,
        });
    }

    function toggleGuardian(m) {
        const h = state.detail;
        app.modals.confirm(
            m.guardian ? `Remove the guardian flag from ${m.name}?` : `Make ${m.name} a guardian?`,
            async () => {
                try {
                    await app.pb.send(`/api/households/${h.id}/members/${m.id}/guardian`, { method: m.guardian ? "DELETE" : "POST" });
                    state.error = "";
                    await loadDetail();
                } catch (err) {
                    failed(err);
                }
            },
        );
    }

    function removeMember(m) {
        const h = state.detail;
        app.modals.confirm(`Remove ${m.name} from ${h.name}? This deletes the person's row.`, async () => {
            try {
                await app.pb.send(`/api/households/${h.id}/members/${m.id}`, { method: "DELETE" });
                state.error = "";
                await loadDetail();
            } catch (err) {
                failed(err);
            }
        });
    }

    // ---- merge, split, duplicates -------------------------------------------
    async function sendOrExplain(path, opts, routeName) {
        try {
            return await app.pb.send(path, opts);
        } catch (err) {
            // the modal shows err.response.message
            const text = householdError(err, routeName);
            err.response = { ...(err.response || {}), message: text };
            throw err;
        }
    }

    function mergeInto(target, source) {
        app.modals.confirm(
            `Merge ${source.name} into ${target.name}? The people of ${source.name} move to ${target.name}. This cannot be undone here.`,
            async () => {
                try {
                    await app.pb.send(`/api/households/${target.id}/merge`, { method: "POST", body: { from: source.id } });
                    state.error = "";
                    state.notice = `${source.name} was merged into ${target.name}.`;
                    state.activeId = target.id;
                    await reload();
                } catch (err) {
                    failed(err, "Merge");
                }
            },
        );
    }

    function mergeDialog() {
        const h = state.detail;
        const others = state.households.filter((x) => x.id != h.id && x.status == "active");
        if (!others.length) {
            state.error = "There is no other active household in this organization to merge.";
            return;
        }
        app.modals.openOrgForm({
            title: `Merge a household into ${h.name}`,
            submitLabel: "Merge",
            fields: [{
                id: "from",
                label: "Household to merge in",
                type: "select",
                options: others.map((x) => ({ value: x.id, label: x.name })),
                value: others[0].id,
                hint: "Its people move here. The other household is closed.",
            }],
            onsubmit: async (v) => {
                await sendOrExplain(`/api/households/${h.id}/merge`, { method: "POST", body: { from: v.from } }, "Merge");
                state.notice = `${others.find((x) => x.id == v.from)?.name} was merged into ${h.name}.`;
            },
            onsave: reload,
        });
    }

    function splitDialog() {
        const h = state.detail;
        const people = (h.members || []).filter((m) => m.status == "active");
        app.modals.openOrgForm({
            title: `Split ${h.name}`,
            submitLabel: "Split",
            fields: [
                { id: "name", label: "Name of the new household", required: true },
                ...people.map((m) => ({ id: "m_" + m.id, label: `Move ${m.name} (${m.role})`, type: "checkbox", value: false })),
            ],
            onsubmit: async (v) => {
                const members = people.filter((m) => v["m_" + m.id]).map((m) => m.id);
                if (!members.length) {
                    throw new Error("Choose at least one person to move.");
                }
                const res = await sendOrExplain(`/api/households/${h.id}/split`, { method: "POST", body: { members, name: v.name } }, "Split");
                state.notice = `Created ${res?.to?.name || v.name} with ${members.length} ${members.length == 1 ? "person" : "people"}.`;
                if (res?.to?.id) {
                    state.activeId = res.to.id;
                }
            },
            onsave: reload,
        });
    }

    async function findDuplicates() {
        state.isLoadingDuplicates = true;
        state.error = "";
        try {
            const res = await app.pb.send("/api/households/duplicates", { query: { org: state.orgId } });
            state.duplicates = normalizeCandidates(res);
        } catch (err) {
            state.duplicates = null;
            failed(err, "Duplicate detection");
        }
        state.isLoadingDuplicates = false;
    }

    function hhName(id) {
        return state.households.find((x) => x.id == id)?.name || "";
    }

    function mergeCandidate(c) {
        const target = state.households.find((x) => x.id == c.a.household);
        const source = state.households.find((x) => x.id == c.b.household);
        if (!target || !source || target.id == source.id) {
            state.error = "This candidate does not name two different households of this organization.";
            return;
        }
        mergeInto(target, source);
    }

    // ---- view ---------------------------------------------------------------
    function btn(icon, title, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    function flag(text, cls = "") {
        return t.span({ className: `label sm ${cls}` }, text);
    }

    function detailView() {
        const h = state.detail;
        if (!h) {
            return t.div(
                { className: "txt-hint txt-center m-t-20" },
                state.isLoadingDetail ? t.span({ className: "skeleton-loader" }) : "Choose a household to see its people.",
            );
        }
        const addr = h.address && typeof h.address == "object" ? Object.values(h.address).filter(Boolean).join(", ") : "";
        return t.div(
            { id: "household_detail", className: "m-t-20" },
            t.div(
                { className: "flex m-b-10" },
                t.h5({ className: "m-r-auto" }, h.name),
                t.div(
                    { className: "inline-flex gap-xs" },
                    t.button({ type: "button", className: "btn sm secondary", onclick: editHousehold }, t.span({ className: "txt" }, "Edit")),
                    t.button(
                        { type: "button", className: "btn sm secondary", onclick: () => setHouseholdStatus(h.status == "archived" ? "active" : "archived") },
                        t.span({ className: "txt" }, h.status == "archived" ? "Restore" : "Archive"),
                    ),
                    t.button({ type: "button", className: "btn sm secondary", onclick: mergeDialog }, t.span({ className: "txt" }, "Merge another in")),
                    t.button({ type: "button", className: "btn sm secondary", onclick: splitDialog }, t.span({ className: "txt" }, "Split")),
                    t.button({ type: "button", className: "btn sm", onclick: addMember }, t.i({ className: "ri-user-add-line", ariaHidden: true }), t.span({ className: "txt" }, "Add person")),
                ),
            ),
            t.div({ className: "txt-hint txt-sm m-b-10" }, [addr || "No address", h.homePhone ? " | " + h.homePhone : "", ` | ${h.status}`].join("")),
            t.table(
                { className: "household-members-table" },
                t.thead(
                    null,
                    t.tr(
                        null,
                        t.th(null, "Name"),
                        t.th(null, "Role"),
                        t.th(null, "Flags"),
                        t.th(null, "Birth date"),
                        t.th(null, "Allergies / medical"),
                        t.th(null, "Login"),
                        t.th({ className: "col-meta" }, "Actions"),
                    ),
                ),
                t.tbody(null, () => {
                    const members = state.detail?.members || [];
                    if (!members.length) {
                        return t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No people.")));
                    }
                    return members.map((m) =>
                        t.tr(
                            { "html-data-member-id": m.id },
                            t.td(null, t.span({ className: "txt-bold" }, m.name)),
                            t.td(null, m.role),
                            t.td(
                                null,
                                t.div(
                                    { className: "inline-flex gap-xs" },
                                    m.guardian ? flag("guardian", "success") : null,
                                    m.isMinor ? flag("minor") : null,
                                    m.status != "active" ? flag(m.status, "warning") : null,
                                    m.photo ? flag("photo") : null,
                                ),
                            ),
                            t.td(null, m.birthDate || t.span({ className: "txt-hint" }, "-")),
                            t.td(
                                null,
                                [m.allergies, m.medical].filter(Boolean).join(" / ") || t.span({ className: "txt-hint" }, "-"),
                            ),
                            t.td(null, m.user ? t.code(null, m.user) : t.span({ className: "txt-hint" }, "none")),
                            t.td(
                                { className: "col-meta" },
                                t.div(
                                    { className: "inline-flex gap-xs" },
                                    btn("ri-pencil-line", "Edit " + m.name, () => editMember(m)),
                                    btn(m.guardian ? "ri-shield-line" : "ri-shield-user-line", (m.guardian ? "Remove guardian flag of " : "Make guardian: ") + m.name, () => toggleGuardian(m)),
                                    btn("ri-delete-bin-7-line", "Remove " + m.name, () => removeMember(m), "danger"),
                                ),
                            ),
                        )
                    );
                }),
            ),
        );
    }

    function duplicatesView() {
        return t.div(
            { id: "household_duplicates", className: "m-t-30" },
            t.div(
                { className: "flex m-b-10" },
                t.h6({ className: "m-r-auto" }, "Possible duplicates"),
                t.button(
                    {
                        type: "button",
                        className: () => `btn sm secondary ${state.isLoadingDuplicates ? "loading" : ""}`,
                        disabled: () => !state.orgId || state.isLoadingDuplicates,
                        onclick: findDuplicates,
                    },
                    t.i({ className: "ri-search-eye-line", ariaHidden: true }),
                    t.span({ className: "txt" }, "Find duplicates"),
                ),
            ),
            () => {
                if (!state.duplicates) {
                    return t.div({ className: "txt-hint txt-sm" }, "Looks for the same person in two households of this organization.");
                }
                if (!state.duplicates.length) {
                    return t.div({ className: "txt-hint txt-sm" }, "No candidates found.");
                }
                return t.table(
                    { className: "household-duplicates-table" },
                    t.thead(null, t.tr(null, t.th(null, "Person"), t.th(null, "Household"), t.th(null, "Person"), t.th(null, "Household"), t.th(null, "Reason"), t.th({ className: "col-meta" }, "Actions"))),
                    t.tbody(
                        null,
                        state.duplicates.map((c) =>
                            t.tr(
                                null,
                                t.td(null, c.a.name || c.a.member),
                                t.td(null, c.a.householdName || hhName(c.a.household) || c.a.household),
                                t.td(null, c.b.name || c.b.member),
                                t.td(null, c.b.householdName || hhName(c.b.household) || c.b.household),
                                t.td(null, c.reason),
                                t.td(
                                    { className: "col-meta" },
                                    t.button({ type: "button", className: "btn sm secondary", onclick: () => mergeCandidate(c) }, t.span({ className: "txt" }, "Merge second into first")),
                                ),
                            )
                        ),
                    ),
                );
            },
        );
    }

    const watchers = [];

    return t.div(
        {
            pbEvent: "pageHouseholds",
            className: "page page-households",
            onmount() {
                loadOrgs().then(reload);
                let lastOrg = state.orgId;
                watchers.push(
                    watch(() => state.orgId, () => {
                        app.utils.replaceHashQueryParams({ org: state.orgId });
                        if (state.orgId == lastOrg) {
                            return;
                        }
                        lastOrg = state.orgId;
                        state.activeId = "";
                        state.detail = null;
                        state.notice = "";
                        loadHouseholds();
                    }),
                    watch(() => state.activeId, () => {
                        app.utils.replaceHashQueryParams({ org: state.orgId, household: state.activeId });
                        loadDetail();
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
                t.nav({ className: "breadcrumbs" }, t.div(null, "Households")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({ onclick: reload }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", disabled: () => !state.orgId, onclick: newHousehold },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New household"),
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
                                { id: "households_org", onchange: (e) => state.orgId = e.target.value },
                                () => state.orgs.length
                                    ? state.orgs.map((o) => t.option({ value: o.id, selected: () => state.orgId == o.id }, o.name))
                                    : t.option({ value: "" }, "(no organizations)"),
                            ),
                            t.label({ htmlFor: "households_org" }, "Organization"),
                        ),
                    ),
                    t.div(
                        { className: "col-lg-3" },
                        t.div(
                            { className: "field" },
                            t.select(
                                { id: "households_status", onchange: (e) => state.statusFilter = e.target.value },
                                ["all", "active", "archived"].map((s) => t.option({ value: s, selected: () => state.statusFilter == s }, s)),
                            ),
                            t.label({ htmlFor: "households_status" }, "Status"),
                        ),
                    ),
                ),
                () => state.error ? t.div({ className: "alert danger m-b-10", id: "households_error", role: "alert" }, state.error) : null,
                () => state.notice ? t.div({ className: "alert success m-b-10", id: "households_notice", role: "status" }, state.notice) : null,
                t.table(
                    { className: "households-table" },
                    t.thead(null, t.tr(null, t.th(null, "Name"), t.th(null, "Status"), t.th(null, "Updated"))),
                    t.tbody(null, () => {
                        const rows = visible();
                        if (!rows.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    state.isLoading
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div({ className: "txt-center txt-hint" }, state.orgId ? "No households." : "Create an organization first."),
                                ),
                            );
                        }
                        return rows.map((h) =>
                            t.tr(
                                {
                                    className: () => `handle ${state.activeId == h.id ? "active" : ""}`,
                                    "html-data-household-id": h.id,
                                    tabindex: "0",
                                    onclick: () => state.activeId = h.id,
                                    onkeydown: (e) => {
                                        if (e.key == "Enter" || e.key == " ") {
                                            e.preventDefault();
                                            state.activeId = h.id;
                                        }
                                    },
                                },
                                t.td(null, t.span({ className: "txt-bold" }, h.name)),
                                t.td(null, t.span({ className: `label sm ${h.status == "active" ? "success" : ""}` }, h.status)),
                                t.td(null, String(h.updated || "").slice(0, 10)),
                            )
                        );
                    }),
                ),
                () => state.households.length >= 200 ? t.div({ className: "txt-hint txt-sm m-t-5" }, "Showing the first 200 households.") : null,
                () => detailView(),
                duplicatesView(),
            ),
        ),
    );
}
