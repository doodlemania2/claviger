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

    function newOrg() {
        app.modals.openOrgForm({
            title: "New organization",
            submitLabel: "Create",
            fields: [
                { id: "name", label: "Name", required: true },
                { id: "slug", label: "Slug", hint: "Lowercase letters, digits and dashes. Derived from the name when empty." },
                { id: "owner", label: "Owner (email or user id)", hint: "Optional. Without an owner only superusers can manage the organization." },
                { id: "parent", label: "Parent organization id", hint: "Optional. One level of hierarchy." },
            ],
            onsubmit: (v) => app.pb.send("/api/orgs", { method: "POST", body: v }),
            onsave: () => loadOrgs(),
        });
    }

    function renameOrg(org) {
        app.modals.openOrgForm({
            title: "Edit organization",
            fields: [
                { id: "name", label: "Name", value: org.name, required: true },
                { id: "slug", label: "Slug", value: org.slug, required: true },
            ],
            onsubmit: (v) => app.pb.send(`/api/orgs/${org.id}`, { method: "PATCH", body: v }),
            onsave: () => loadOrgs(),
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
                loadOrgs().then(loadMembers).then(loadInvitations).then(loadScim);
                watchers.push(
                    watch(() => state.activeId, () => {
                        app.utils.replaceHashQueryParams({ org: state.activeId });
                        loadMembers();
                        loadInvitations();
                        loadScim();
                    }),
                    watch(() => state.showArchived, () => loadOrgs()),
                    watch(() => state.showRules, (v) => {
                        if (v && !state.presets.length) {
                            loadPresets();
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
                t.table(
                    { className: "orgs-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th(null, "Name"),
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

                        return state.orgs.map((o) =>
                            t.tr(
                                {
                                    className: () => `handle ${state.activeId == o.id ? "active" : ""}`,
                                    onclick: () => state.activeId = o.id,
                                },
                                t.td(null, t.div({ className: "txt-bold" }, o.name), o.parent ? t.div({ className: "txt-hint txt-sm" }, "child of " + o.parent) : null),
                                t.td(null, o.slug),
                                t.td(null, t.span({ className: `label sm ${o.archived ? "" : "success"}` }, o.archived ? "archived" : "active")),
                                t.td(
                                    { className: "col-meta", onclick: (e) => e.stopPropagation() },
                                    t.div(
                                        { className: "inline-flex gap-xs" },
                                        btn("ri-pencil-line", "Edit", () => renameOrg(o)),
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
