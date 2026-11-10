// Fork-local (Claviger #79a): organizations, their members and the rule
// templates for org-owned collections. Everything goes through /api/orgs, so
// the last-owner and role guards apply here too.

const ROLES = ["owner", "admin", "member", "viewer"];
const STATUSES = ["active", "suspended"];

export function pageOrgs(route) {
    app.store.title = "Organizations";

    const state = store({
        orgs: [],
        members: [],
        invitations: [],
        scimTokens: [],
        scimEndpoint: "",
        scimSettings: null,
        newScimToken: "",
        presets: [],
        showArchived: false,
        search: "",
        activeId: route.query.org?.[0] || "",
        isLoading: false,
        isLoadingMembers: false,
        showRules: false,
        // #84: tree view, hierarchy config, status counts, rule snippets
        collapsed: {},
        config: null,
        status: null,
        statusError: "",
        collections: [],
        snippet: { collection: "", field: "org", roles: [], maxDepth: "", tag: "", result: null, error: "" },
    });

    const active = () => state.orgs.find((o) => o.id == state.activeId);

    async function loadOrgs() {
        state.isLoading = true;
        try {
            const res = await app.pb.send("/api/orgs", {
                query: { archived: state.showArchived ? "true" : "false", q: state.search, perPage: 200 },
            });
            state.orgs = res.items;
            if (!state.orgs.find((o) => o.id == state.activeId)) {
                state.activeId = state.orgs[0]?.id || "";
            }
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
        state.isLoading = false;
    }

    async function loadMembers() {
        const org = active();
        if (!org) {
            state.members = [];
            return;
        }
        state.isLoadingMembers = true;
        try {
            const res = await app.pb.send(`/api/orgs/${org.id}/members`, { query: { perPage: 200 } });
            state.members = res.items;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
        state.isLoadingMembers = false;
    }

    // #79b: invitations of the active org
    async function loadInvitations() {
        const org = active();
        if (!org || org.archived) {
            state.invitations = [];
            return;
        }
        try {
            const res = await app.pb.send(`/api/orgs/${org.id}/invitations`, { query: { perPage: 200 } });
            state.invitations = res.items;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    function inviteMember() {
        const org = active();
        app.modals.openOrgForm({
            title: "Invite by email",
            submitLabel: "Send invitation",
            fields: [
                { id: "email", label: "Email", required: true },
                { id: "role", label: "Role", type: "select", options: ROLES, value: "member" },
                { id: "tags", label: "Tags (comma separated)", hint: "Applied when the invitation is accepted." },
            ],
            onsubmit: (v) =>
                app.pb.send(`/api/orgs/${org.id}/invitations`, {
                    method: "POST",
                    body: { email: v.email, role: v.role, tags: splitTags(v.tags) },
                }),
            onsave: () => loadInvitations(),
        });
    }

    async function resendInvitation(inv) {
        const org = active();
        try {
            const res = await app.pb.send(`/api/orgs/${org.id}/invitations/${inv.id}/resend`, { method: "POST" });
            if (res.emailSent === false) {
                app.toasts.error("Renewed, but the email could not be sent.");
            } else {
                app.toasts.success("Invitation sent again.");
            }
            await loadInvitations();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function revokeInvitation(inv) {
        const org = active();
        app.modals.confirm(`Revoke the invitation for ${inv.email}?`, async () => {
            try {
                await app.pb.send(`/api/orgs/${org.id}/invitations/${inv.id}`, { method: "DELETE" });
                await loadInvitations();
            } catch (err) {
                app.checkApiError(err);
            }
        });
    }

    // #79c: SCIM tokens and settings of the active org
    async function loadScim() {
        const org = active();
        state.newScimToken = "";
        if (!org || org.archived) {
            state.scimTokens = [];
            state.scimSettings = null;
            return;
        }
        try {
            const [tokens, settings] = await Promise.all([
                app.pb.send(`/api/orgs/${org.id}/scim-tokens`),
                app.pb.send(`/api/orgs/${org.id}/scim-settings`),
            ]);
            state.scimTokens = tokens.items;
            state.scimEndpoint = tokens.endpoint;
            state.scimSettings = settings;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    function createScimToken() {
        const org = active();
        app.modals.openOrgForm({
            title: "New SCIM token",
            submitLabel: "Create token",
            fields: [{ id: "name", label: "Name", required: true, hint: "For example Entra ID production." }],
            onsubmit: async (v) => {
                const res = await app.pb.send(`/api/orgs/${org.id}/scim-tokens`, { method: "POST", body: { name: v.name } });
                state.newScimToken = res.token;
            },
            onsave: () => {
                const shown = state.newScimToken;
                loadScim().then(() => state.newScimToken = shown);
            },
        });
    }

    function revokeScimToken(tok) {
        const org = active();
        app.modals.confirm(`Revoke the token "${tok.name}"? The identity provider stops working until it gets a new one.`, async () => {
            try {
                await app.pb.send(`/api/orgs/${org.id}/scim-tokens/${tok.id}`, { method: "DELETE" });
                await loadScim();
            } catch (err) {
                app.checkApiError(err);
            }
        });
    }

    function editScimSettings() {
        const org = active();
        const cur = state.scimSettings || { defaultRole: "member", groupRoles: {}, deactivationBans: false };
        app.modals.openOrgForm({
            title: "SCIM settings",
            submitLabel: "Save",
            fields: [
                { id: "defaultRole", label: "Role of new users", type: "select", options: ["admin", "member", "viewer"], value: cur.defaultRole },
                {
                    id: "groupRoles",
                    label: "Groups that grant a role",
                    hint: "Group name = role, separated by semicolons. Owner cannot be granted by SCIM.",
                    value: Object.entries(cur.groupRoles || {}).map(([g, r]) => `${g} = ${r}`).join("; "),
                },
                { id: "deactivationBans", label: "Also ban the account on deactivation", type: "checkbox", value: !!cur.deactivationBans },
            ],
            onsubmit: (v) => {
                const groupRoles = {};
                for (const part of (v.groupRoles || "").split(";")) {
                    const i = part.lastIndexOf("=");
                    if (i > 0) {
                        groupRoles[part.slice(0, i).trim()] = part.slice(i + 1).trim();
                    } else if (part.trim()) {
                        throw new Error('Use "Group name = role".');
                    }
                }
                return app.pb.send(`/api/orgs/${org.id}/scim-settings`, {
                    method: "PUT",
                    body: { defaultRole: v.defaultRole, groupRoles, deactivationBans: v.deactivationBans },
                });
            },
            onsave: () => loadScim(),
        });
    }

    async function loadPresets() {
        try {
            const res = await app.pb.send("/api/orgs/rule-templates");
            state.presets = res.presets;
        } catch (err) {
            app.checkApiError(err);
        }
    }

    // ---- #84: hierarchy ---------------------------------------------------

    async function loadConfig() {
        try {
            state.config = await app.pb.send("/api/orgs/config");
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    async function loadStatus() {
        const org = active();
        state.status = null;
        state.statusError = "";
        if (!org) {
            return;
        }
        try {
            const res = await app.pb.send(`/api/orgs/${org.id}/status`);
            if (org.id == state.activeId) {
                state.status = res;
            }
        } catch (err) {
            if (!err.isAbort) {
                state.statusError = err?.response?.message || "The status could not be loaded.";
            }
        }
    }

    // the loaded orgs as visible tree rows: [{ org, level, hasKids }]
    function treeRows() {
        const byId = new Map(state.orgs.map((o) => [o.id, o]));
        const kids = {};
        const roots = [];
        for (const o of state.orgs) {
            if (o.parent && byId.has(o.parent)) {
                (kids[o.parent] ||= []).push(o);
            } else {
                roots.push(o);
            }
        }
        const rows = [];
        const seen = new Set();
        const walk = (list, level) => {
            for (const o of list) {
                if (seen.has(o.id)) {
                    continue;
                }
                seen.add(o.id);
                const children = kids[o.id] || [];
                rows.push({ org: o, level, hasKids: children.length > 0 });
                if (!state.collapsed[o.id]) {
                    walk(children, level + 1);
                }
            }
        };
        walk(roots, 0);
        return rows;
    }

    function orgName(id) {
        return state.orgs.find((o) => o.id == id)?.name || id;
    }

    function toggleCollapsed(o) {
        state.collapsed = { ...state.collapsed, [o.id]: !state.collapsed[o.id] };
    }

    function setAllCollapsed(v) {
        state.collapsed = v ? Object.fromEntries(state.orgs.map((o) => [o.id, true])) : {};
    }

    function kindSuggestions() {
        const all = new Set(state.config?.kinds || []);
        state.orgs.forEach((o) => o.kind && all.add(o.kind));
        return [...all].sort();
    }

    // options for a parent select: every loaded org, indented by its place in the tree
    function parentOptions(exceptId) {
        const opts = [{ value: "", label: "(none, a top level organization)" }];
        const byId = new Map(state.orgs.map((o) => [o.id, o]));
        const levelOf = (o) => {
            let n = 0;
            for (let cur = o; cur?.parent && byId.has(cur.parent) && n < 10; cur = byId.get(cur.parent)) {
                n++;
            }
            return n;
        };
        for (const o of state.orgs) {
            if (o.id != exceptId) {
                opts.push({ value: o.id, label: "  ".repeat(levelOf(o)) + o.name + (o.kind ? ` (${o.kind})` : "") });
            }
        }
        return opts;
    }

    function moveOrg(org) {
        const max = state.config?.maxDepth;
        app.modals.openOrgForm({
            title: `Move ${org.name}`,
            submitLabel: "Move",
            fields: [{
                id: "parent",
                label: "New parent",
                type: "select",
                options: parentOptions(org.id),
                value: org.parent || "",
                hint: `The whole subtree moves with it. A tree can have at most ${max ?? "a configured number of"} levels, and an organization cannot sit below itself.`,
            }],
            onsubmit: (v) => app.pb.send(`/api/orgs/${org.id}`, { method: "PATCH", body: { parent: v.parent } }),
            onsave: () => loadOrgs().then(loadStatus).then(loadConfig),
        });
    }

    // ---- #84: rule snippets -----------------------------------------------

    async function loadSnippetCollections() {
        try {
            const cols = await app.pb.collections.getFullList({ requestKey: "orgs_snippet_cols" });
            const orgsCol = cols.find((c) => c.name == "_orgs");
            state.collections = cols
                .filter((c) => !c.system || c.name == "users")
                .map((c) => ({
                    name: c.name,
                    fields: (c.fields || []).filter((f) => f.type == "relation" && orgsCol && f.collectionId == orgsCol.id).map((f) => f.name),
                }))
                .filter((c) => c.fields.length);
        } catch (err) {
            if (!err.isAbort) {
                console.warn("orgs: failed to load collections", err);
            }
        }
    }

    async function refreshSnippet() {
        const sn = state.snippet;
        sn.error = "";
        try {
            sn.result = await app.pb.send("/api/orgs/ancestor-rule", {
                query: { field: sn.field, roles: sn.roles.join(","), maxDepth: sn.maxDepth, tag: sn.tag },
                requestKey: "orgs_snippet",
            });
        } catch (err) {
            if (!err.isAbort) {
                sn.result = null;
                sn.error = err?.response?.message || "The rule could not be generated.";
            }
        }
    }

    function toggleRole(role, on) {
        const sn = state.snippet;
        sn.roles = ROLES.filter((r) => (r == role ? on : sn.roles.includes(r)));
        refreshSnippet();
    }

    function snippetPanel() {
        const sn = state.snippet;
        return t.div(
            { className: "org-snippets m-t-10", id: "org_snippets" },
            t.p(
                { className: "txt-hint" },
                "Oversight lets members of an organization's ancestors read its records. It is opt-in per collection: ",
                "paste the List and View rules below only where higher organizations should see the data, never on pastoral data. ",
                "Writes stay with the record's own organization. The text comes from the same code the server uses.",
            ),
            t.div(
                { className: "grid" },
                t.div(
                    { className: "col-lg-4" },
                    t.div(
                        { className: "field" },
                        t.select(
                            {
                                id: "snippet_collection",
                                onchange: (e) => {
                                    const [coll, field] = e.target.value.split("|");
                                    sn.collection = coll || "";
                                    if (field) {
                                        sn.field = field;
                                    }
                                    refreshSnippet();
                                },
                            },
                            t.option({ value: "" }, "(choose a collection, or type a field below)"),
                            () => state.collections.flatMap((c) => c.fields.map((f) => t.option({ value: `${c.name}|${f}` }, `${c.name} (${f})`))),
                        ),
                        t.label({ htmlFor: "snippet_collection" }, "Collection"),
                    ),
                ),
                t.div(
                    { className: "col-lg-4" },
                    t.div(
                        { className: "field" },
                        t.input({
                            type: "text",
                            id: "snippet_field",
                            value: () => sn.field,
                            oninput: (e) => {
                                sn.field = e.target.value.trim();
                                refreshSnippet();
                            },
                        }),
                        t.label({ htmlFor: "snippet_field" }, "Relation field to the organization"),
                    ),
                ),
                t.div(
                    { className: "col-lg-2" },
                    t.div(
                        { className: "field" },
                        t.select(
                            {
                                id: "snippet_depth",
                                onchange: (e) => {
                                    sn.maxDepth = e.target.value;
                                    refreshSnippet();
                                },
                            },
                            t.option({ value: "" }, "any level"),
                            [1, 2, 3, 4, 5, 6, 7, 8].map((n) => t.option({ value: String(n) }, `${n} up`)),
                        ),
                        t.label({ htmlFor: "snippet_depth" }, "Ancestors within"),
                    ),
                ),
                t.div(
                    { className: "col-lg-2" },
                    t.div(
                        { className: "field" },
                        t.input({
                            type: "text",
                            id: "snippet_tag",
                            value: () => sn.tag,
                            oninput: (e) => {
                                sn.tag = e.target.value.trim();
                                refreshSnippet();
                            },
                        }),
                        t.label({ htmlFor: "snippet_tag" }, "Tag (optional)"),
                    ),
                ),
            ),
            t.div(
                { className: "inline-flex gap-sm m-b-10" },
                t.span({ className: "txt-hint txt-sm" }, "Roles at the ancestor (none ticked = any role):"),
                ROLES.map((r) =>
                    t.div(
                        { className: "field inline" },
                        t.input({
                            type: "checkbox",
                            id: "snippet_role_" + r,
                            className: "switch",
                            checked: () => sn.roles.includes(r),
                            onchange: (e) => toggleRole(r, e.target.checked),
                        }),
                        t.label({ htmlFor: "snippet_role_" + r }, r),
                    )
                ),
            ),
            () => sn.error ? t.div({ className: "alert danger", id: "snippet_error" }, sn.error) : null,
            () =>
                sn.result
                    ? t.div(
                        { id: "snippet_result" },
                        t.div({ className: "txt-bold" }, () => sn.collection ? `Rules for ${sn.collection}` : "Rules"),
                        ruleBlock("List rule and View rule", sn.result.rule),
                        ruleBlock("Create rule (the record's own organization)", sn.result.ruleSet.createRule),
                        ruleBlock("Update rule", sn.result.ruleSet.updateRule),
                        ruleBlock("Delete rule", sn.result.ruleSet.deleteRule),
                    )
                    : null,
        );
    }

    function newOrg() {
        app.modals.openOrgForm({
            title: "New organization",
            submitLabel: "Create",
            fields: [
                { id: "name", label: "Name", required: true },
                { id: "slug", label: "Slug", hint: "Lowercase letters, digits and dashes. Derived from the name when empty." },
                { id: "owner", label: "Owner (email or user id)", hint: "Optional. Without an owner only superusers can manage the organization." },
                { id: "parent", label: "Parent organization", type: "select", options: parentOptions(""), value: "", hint: "Optional. A tree can have at most the number of levels shown above." },
                { id: "kind", label: "Kind", suggestions: kindSuggestions(), hint: "Optional free label such as region, congregation or ministry. Pick your own vocabulary." },
            ],
            onsubmit: (v) => app.pb.send("/api/orgs", { method: "POST", body: v }),
            onsave: () => loadOrgs().then(loadStatus).then(loadConfig),
        });
    }

    function renameOrg(org) {
        app.modals.openOrgForm({
            title: "Edit organization",
            fields: [
                { id: "name", label: "Name", value: org.name, required: true },
                { id: "slug", label: "Slug", value: org.slug, required: true },
                { id: "kind", label: "Kind", value: org.kind || "", suggestions: kindSuggestions(), hint: "Optional free label. Empty clears it." },
            ],
            onsubmit: (v) => app.pb.send(`/api/orgs/${org.id}`, { method: "PATCH", body: v }),
            onsave: () => loadOrgs().then(loadStatus).then(loadConfig),
        });
    }

    function setArchived(org, archived) {
        app.modals.confirm(
            archived
                ? `Archive ${org.name}? Its members lose access through the API rules until it is restored.`
                : `Restore ${org.name}?`,
            async () => {
                try {
                    await app.pb.send(`/api/orgs/${org.id}`, { method: "PATCH", body: { archived } });
                    await loadOrgs();
                } catch (err) {
                    app.checkApiError(err);
                }
            },
        );
    }

    function deleteOrg(org) {
        app.modals.openOrgForm({
            title: "Delete organization permanently",
            submitLabel: "Delete",
            fields: [{
                id: "confirm",
                label: `Type ${org.slug} to confirm`,
                hint: "Deletes the organization, its memberships and tags. Refused while records still belong to it.",
                required: true,
            }],
            onsubmit: (v) => app.pb.send(`/api/orgs/${org.id}`, { method: "DELETE", body: v }),
            onsave: () => loadOrgs(),
        });
    }

    function addMember() {
        const org = active();
        app.modals.openOrgForm({
            title: "Add member",
            submitLabel: "Add",
            fields: [
                { id: "user", label: "User (email or id)", required: true },
                { id: "role", label: "Role", type: "select", options: ROLES.slice(1), value: "member" },
                { id: "tags", label: "Tags (comma separated)", hint: "Free-form labels such as youth leader." },
            ],
            onsubmit: (v) =>
                app.pb.send(`/api/orgs/${org.id}/members`, {
                    method: "POST",
                    body: { user: v.user, role: v.role, tags: splitTags(v.tags) },
                }),
            onsave: () => loadMembers(),
        });
    }

    function editMember(m) {
        const org = active();
        app.modals.openOrgForm({
            title: `Edit ${m.email || m.name || m.user}`,
            fields: [
                { id: "role", label: "Role", type: "select", options: ROLES, value: m.role },
                { id: "status", label: "Status", type: "select", options: STATUSES, value: m.status },
                { id: "tags", label: "Tags (comma separated)", value: (m.tags || []).join(", ") },
            ],
            onsubmit: (v) =>
                app.pb.send(`/api/orgs/${org.id}/members/${m.id}`, {
                    method: "PATCH",
                    body: { role: v.role, status: v.status, tags: splitTags(v.tags) },
                }),
            onsave: () => loadMembers(),
        });
    }

    function removeMember(m) {
        const org = active();
        app.modals.confirm(`Remove ${m.email || m.name || m.user} from ${org.name}?`, async () => {
            try {
                await app.pb.send(`/api/orgs/${org.id}/members/${m.id}`, { method: "DELETE" });
                await loadMembers();
            } catch (err) {
                app.checkApiError(err);
            }
        });
    }

    function splitTags(s) {
        return (s || "").split(",").map((v) => v.trim()).filter(Boolean);
    }

    function btn(icon, title, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    function ruleBlock(label, rule) {
        return t.div(
            { className: "org-rule" },
            t.div({ className: "txt-hint txt-sm" }, label),
            t.div(
                { className: "inline-flex gap-xs" },
                t.code({ className: "txt-break" }, rule === null ? "null (superusers only)" : rule),
                rule === null ? null : app.components.copyButton(rule),
            ),
        );
    }

    const watchers = [];

    return t.div(
        {
            pbEvent: "pageOrgs",
            className: "page page-orgs",
            onmount() {
                loadConfig();
                loadOrgs().then(loadMembers).then(loadInvitations).then(loadScim).then(loadStatus);
                watchers.push(
                    watch(() => state.activeId, () => {
                        app.utils.replaceHashQueryParams({ org: state.activeId });
                        loadMembers();
                        loadInvitations();
                        loadScim();
                        loadStatus();
                    }),
                    watch(() => state.showArchived, () => loadOrgs()),
                    watch(() => state.showRules, (v) => {
                        if (v && !state.presets.length) {
                            loadPresets();
                        }
                        if (v) {
                            loadSnippetCollections();
                            refreshSnippet();
                        }
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
                t.nav({ className: "breadcrumbs" }, t.div(null, "Organizations")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    t.div(
                        { className: "field inline" },
                        t.input({
                            type: "checkbox",
                            id: "orgs_archived",
                            className: "switch",
                            checked: () => state.showArchived,
                            onchange: (e) => state.showArchived = e.target.checked,
                        }),
                        t.label({ htmlFor: "orgs_archived" }, "Archived"),
                    ),
                    app.components.refreshButton({ onclick: () => loadOrgs().then(loadMembers) }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", onclick: newOrg },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New organization"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                t.div(
                    { className: "txt-hint txt-sm m-b-10", id: "orgs_depth", hidden: () => !state.config },
                    () =>
                        state.config
                            ? `Maximum tree depth: ${state.config.maxDepth} levels (a top level organization is level 1). Set by ${state.config.envVar} on the server: default ${state.config.defaultDepth}, hard cap ${state.config.hardMaxDepth}. Read-only here.`
                            : "",
                ),
                t.div(
                    { className: "inline-flex gap-xs m-b-10" },
                    t.button({ type: "button", className: "btn sm secondary", onclick: () => setAllCollapsed(false) }, "Expand all"),
                    t.button({ type: "button", className: "btn sm secondary", onclick: () => setAllCollapsed(true) }, "Collapse all"),
                ),
                t.table(
                    { className: "orgs-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Name"),
                            t.th(null, "Kind"),
                            t.th(null, "Slug"),
                            t.th(null, "State"),
                            t.th({ className: "col-meta" }, "Actions"),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.orgs.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    state.isLoading
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div(
                                            { className: "txt-center txt-hint" },
                                            "No organizations. Nothing changes for your users until you create one.",
                                        ),
                                ),
                            );
                        }

                        return treeRows().map(({ org: o, level, hasKids }) =>
                            t.tr(
                                {
                                    className: () => `handle ${state.activeId == o.id ? "active" : ""}`,
                                    onclick: () => state.activeId = o.id,
                                    "html-data-org-id": o.id,
                                    "html-data-level": String(level + 1),
                                },
                                t.td(
                                    null,
                                    t.div(
                                        { className: "inline-flex gap-xs org-tree-cell", style: `padding-left:${level * 22}px` },
                                        hasKids
                                            ? t.button(
                                                {
                                                    type: "button",
                                                    className: "btn sm transparent circle org-toggle",
                                                    ariaLabel: (state.collapsed[o.id] ? "Expand " : "Collapse ") + o.name,
                                                    ariaExpanded: () => String(!state.collapsed[o.id]),
                                                    onclick: (e) => {
                                                        e.stopPropagation();
                                                        toggleCollapsed(o);
                                                    },
                                                },
                                                t.i({ className: state.collapsed[o.id] ? "ri-arrow-right-s-line" : "ri-arrow-down-s-line", ariaHidden: true }),
                                            )
                                            : t.span({ className: "org-toggle-spacer" }),
                                        t.div(
                                            null,
                                            t.div({ className: "txt-bold" }, o.name),
                                            o.parent ? t.div({ className: "txt-hint txt-sm" }, "under " + orgName(o.parent)) : null,
                                        ),
                                    ),
                                ),
                                t.td(null, o.kind ? t.span({ className: "label sm" }, o.kind) : t.span({ className: "txt-hint" }, "-")),
                                t.td(null, o.slug),
                                t.td(null, t.span({ className: `label sm ${o.archived ? "" : "success"}` }, o.archived ? "archived" : "active")),
                                t.td(
                                    { className: "col-meta", onclick: (e) => e.stopPropagation() },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        btn("ri-pencil-line", "Edit", () => renameOrg(o)),
                                        btn("ri-corner-down-right-line", "Move", () => moveOrg(o)),
                                        btn(
                                            o.archived ? "ri-inbox-unarchive-line" : "ri-archive-line",
                                            o.archived ? "Restore" : "Archive",
                                            () => setArchived(o, !o.archived),
                                        ),
                                        o.archived ? btn("ri-delete-bin-7-line", "Delete permanently", () => deleteOrg(o), "danger") : null,
                                    ),
                                ),
                            )
                        );
                    }),
                ),
                t.div(
                    { className: "org-status m-t-10", id: "org_status", hidden: () => !active() },
                    () => {
                        if (state.statusError) {
                            return t.div({ className: "txt-hint txt-sm" }, state.statusError);
                        }
                        const st = state.status;
                        if (!st) {
                            return null;
                        }
                        const kinds = st.descendantsByKind.map((k) => `${k.count} ${k.kind || "without a kind"}`).join(", ");
                        return t.div(
                            { className: "txt-sm" },
                            t.span({ className: "txt-bold" }, `Status of ${active()?.name}: `),
                            `level ${st.level} of ${st.maxDepth}, ${st.height} ${st.height == 1 ? "level" : "levels"} below. `,
                            `${st.descendants} ${st.descendants == 1 ? "organization" : "organizations"} below`,
                            kinds ? ` (${kinds})` : "",
                            st.archivedDescendants ? `, ${st.archivedDescendants} archived` : "",
                            `. Members: ${st.members.active} active, ${st.members.suspended} suspended.`,
                        );
                    },
                ),
                t.div(
                    { className: "flex m-t-20 m-b-10" },
                    t.h6({ className: "m-r-auto" }, () => active() ? `Members of ${active().name}` : "Members"),
                    t.button(
                        { type: "button", className: "btn sm", hidden: () => !active() || active().archived, onclick: addMember },
                        t.i({ className: "ri-user-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Add member"),
                    ),
                ),
                t.table(
                    { className: "org-members-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Member"),
                            t.th(null, "Role"),
                            t.th(null, "Status"),
                            t.th(null, "Tags"),
                            t.th(null, "Source"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.members.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    state.isLoadingMembers
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div({ className: "txt-center txt-hint" }, "No members."),
                                ),
                            );
                        }

                        return state.members.map((m) =>
                            t.tr(
                                null,
                                t.td(null, t.div({ className: "txt-bold" }, m.email || m.name || m.user), m.email && m.name ? t.div({ className: "txt-hint txt-sm" }, m.name) : null),
                                t.td(null, m.role),
                                t.td(null, t.span({ className: `label sm ${m.status == "active" ? "success" : "warning"}` }, m.status)),
                                t.td(null, (m.tags || []).join(", ")),
                                t.td(null, m.source),
                                t.td(
                                    { className: "col-meta" },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        btn("ri-pencil-line", "Edit", () => editMember(m)),
                                        btn("ri-user-unfollow-line", "Remove", () => removeMember(m), "danger"),
                                    ),
                                ),
                            )
                        );
                    }),
                ),
                t.div(
                    { className: "flex m-t-20 m-b-10" },
                    t.h6({ className: "m-r-auto" }, "Invitations"),
                    t.a(
                        {
                            className: "btn sm secondary m-r-5",
                            hidden: () => !active() || active().archived,
                            href: () => `/orgs/${active()?.slug}`,
                            target: "_blank",
                            rel: "noopener",
                        },
                        t.i({ className: "ri-external-link-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Org admin page"),
                    ),
                    t.button(
                        { type: "button", className: "btn sm", hidden: () => !active() || active().archived, onclick: inviteMember },
                        t.i({ className: "ri-mail-send-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Invite"),
                    ),
                ),
                t.table(
                    { className: "org-invitations-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Email"),
                            t.th(null, "Role"),
                            t.th(null, "Tags"),
                            t.th(null, "Status"),
                            t.th(null, "Expires"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.invitations.length) {
                            return t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No invitations.")));
                        }

                        return state.invitations.map((inv) =>
                            t.tr(
                                null,
                                t.td(null, inv.email),
                                t.td(null, inv.role),
                                t.td(null, (inv.tags || []).join(", ")),
                                t.td(
                                    null,
                                    t.span(
                                        { className: `label sm ${inv.status == "accepted" ? "success" : inv.status == "pending" ? "" : "warning"}` },
                                        inv.status,
                                    ),
                                ),
                                t.td(null, inv.status == "accepted" ? "" : String(inv.expires || "").slice(0, 16)),
                                t.td(
                                    { className: "col-meta" },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        inv.status == "pending" || inv.status == "expired" ? btn("ri-mail-send-line", "Resend", () => resendInvitation(inv)) : null,
                                        inv.status == "pending" ? btn("ri-close-line", "Revoke", () => revokeInvitation(inv), "danger") : null,
                                    ),
                                ),
                            )
                        );
                    }),
                ),
                t.div(
                    { className: "flex m-t-20 m-b-10" },
                    t.h6({ className: "m-r-auto" }, "SCIM provisioning"),
                    t.button(
                        { type: "button", className: "btn sm secondary m-r-5", hidden: () => !active() || active().archived, onclick: editScimSettings },
                        t.i({ className: "ri-settings-3-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "Settings"),
                    ),
                    t.button(
                        { type: "button", className: "btn sm", hidden: () => !active() || active().archived, onclick: createScimToken },
                        t.i({ className: "ri-key-2-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New token"),
                    ),
                ),
                t.div(
                    { className: "txt-hint txt-sm m-b-10", hidden: () => !state.scimEndpoint },
                    "Tenant URL: ",
                    t.code(null, () => state.scimEndpoint),
                ),
                () =>
                    state.newScimToken
                        ? t.div(
                            { className: "alert success m-b-10" },
                            t.div(null, "Copy this token now. It is shown only once."),
                            t.div({ className: "inline-flex gap-xs" }, t.code({ className: "txt-break" }, state.newScimToken), app.components.copyButton(state.newScimToken)),
                        )
                        : null,
                t.table(
                    { className: "org-scim-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Name"),
                            t.th(null, "Token"),
                            t.th(null, "Last used"),
                            t.th(null, "Expires"),
                            t.th(null, "Status"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(null, () => {
                        if (!state.scimTokens.length) {
                            return t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No SCIM tokens. Without one, /scim/v2 answers 401.")));
                        }

                        return state.scimTokens.map((tok) =>
                            t.tr(
                                null,
                                t.td(null, tok.name),
                                t.td(null, t.code(null, `scim_${tok.prefix}_...`)),
                                t.td(null, tok.lastUsed ? String(tok.lastUsed).slice(0, 16) : "never"),
                                t.td(null, tok.expires ? String(tok.expires).slice(0, 16) : "never"),
                                t.td(null, t.span({ className: `label sm ${tok.active ? "success" : "warning"}` }, tok.revoked ? "revoked" : tok.active ? "active" : "expired")),
                                t.td(
                                    { className: "col-meta" },
                                    tok.active ? btn("ri-close-line", "Revoke", () => revokeScimToken(tok), "danger") : null,
                                ),
                            )
                        );
                    }),
                ),
                t.div(
                    { className: "m-t-20" },
                    t.button(
                        { type: "button", className: "btn sm secondary", onclick: () => state.showRules = !state.showRules },
                        t.i({ className: "ri-shield-keyhole-line", ariaHidden: true }),
                        t.span({ className: "txt" }, () => state.showRules ? "Hide rule templates" : "Rule templates for org-owned collections"),
                    ),
                ),
                () => state.showRules ? t.h6({ className: "m-t-20" }, "Rule snippets: oversight by ancestor organizations") : null,
                () => state.showRules ? snippetPanel() : null,
                () => {
                    if (!state.showRules) {
                        return null;
                    }
                    return t.div(
                        { className: "org-rules m-t-10" },
                        t.p(
                            { className: "txt-hint" },
                            "A collection is org-owned when it has a required relation field named org that points at _orgs, with an index on it. ",
                            "Paste these into its API rules. Clients should filter on org and send skipTotal=1 for large collections.",
                        ),
                        state.presets.map((p) =>
                            t.div(
                                { className: "m-b-20" },
                                t.div({ className: "txt-bold" }, p.title),
                                t.div({ className: "txt-hint txt-sm m-b-5" }, p.description),
                                ruleBlock("List", p.rules.listRule),
                                ruleBlock("View", p.rules.viewRule),
                                ruleBlock("Create", p.rules.createRule),
                                ruleBlock("Update", p.rules.updateRule),
                                ruleBlock("Delete", p.rules.deleteRule),
                            )
                        ),
                    );
                },
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
