// Fork-local (Claviger #92): the "Single sign-on" section of the organizations page. Everything goes
// through /api/orgs/{id}/sso/..., so the owner/admin guard and the audit trail apply here too.

import { openSsoConnectionModal, openSsoDomainModal } from "./ssoModals";

const when = (v) => {
    const s = String(v || "");
    return s && !s.startsWith("0001") ? s.slice(0, 16) : "";
};

// getOrg: () => the active organization (or undefined). Returns { el, load } ; call load() when the org changes.
export function ssoSection(getOrg) {
    const state = store({ connections: [], domains: [], info: null, error: "", isLoading: false });

    const usable = () => {
        const o = getOrg();
        return !!o && !o.archived;
    };

    async function load() {
        const org = getOrg();
        state.error = "";
        if (!org || org.archived) {
            state.connections = [];
            state.domains = [];
            return;
        }
        state.isLoading = true;
        try {
            const [conns, doms, info] = await Promise.all([
                app.pb.send(`/api/orgs/${org.id}/sso/connections`, { requestKey: "sso_conns" }),
                app.pb.send(`/api/orgs/${org.id}/sso/domains`, { requestKey: "sso_doms" }),
                app.pb.send(`/api/orgs/${org.id}/sso/presets`, { requestKey: "sso_presets" }),
            ]);
            if (org.id == getOrg()?.id) {
                state.connections = conns.items;
                state.domains = doms.items;
                state.info = info;
            }
        } catch (err) {
            if (!err.isAbort) {
                state.error = err?.response?.message || "Single sign-on could not be loaded.";
            }
        }
        state.isLoading = false;
    }

    const connName = (id) => state.connections.find((c) => c.id == id)?.name || "";

    function btn(icon, title, onclick, extra = "") {
        return t.button(
            { type: "button", className: `btn sm secondary circle ${extra}`, title, ariaLabel: title, onclick },
            t.i({ className: icon, ariaHidden: true }),
        );
    }

    async function addConnection() {
        if (!state.info) {
            await load(); // the presets and the callback URL come with the first load
        }
        openSsoConnectionModal(getOrg(), state.info || {}, null, load);
    }

    function editConnection(c) {
        openSsoConnectionModal(getOrg(), state.info || {}, c, load);
    }

    async function setEnabled(c, enabled) {
        try {
            await app.pb.send(`/api/orgs/${getOrg().id}/sso/connections/${c.id}`, { method: "PATCH", body: { enabled } });
            app.toasts.success(enabled ? `${c.name} enabled.` : `${c.name} disabled.`);
            await load();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    async function testConnection(c) {
        try {
            const res = await app.pb.send(`/api/orgs/${getOrg().id}/sso/connections/${c.id}/test`, { method: "POST" });
            if (res.ok) {
                app.toasts.success("The provider answered and its discovery document is valid.");
            } else {
                app.toasts.error(res.error || "The provider check failed.");
            }
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function deleteConnection(c) {
        app.modals.confirm(`Delete the connection "${c.name}"? Its domain claims go with it.`, async () => {
            try {
                await app.pb.send(`/api/orgs/${getOrg().id}/sso/connections/${c.id}`, { method: "DELETE" });
                await load();
            } catch (err) {
                app.checkApiError(err);
            }
        });
    }

    function addDomain() {
        if (!state.connections.length) {
            app.toasts.error("Create a connection first.");
            return;
        }
        openSsoDomainModal(getOrg(), state.connections, load);
    }

    async function verifyDomain(d) {
        try {
            const res = await app.pb.send(`/api/orgs/${getOrg().id}/sso/domains/${d.id}/verify`, { method: "POST" });
            if (res.verified) {
                app.toasts.success(`${d.domain} is verified.`);
            } else {
                app.toasts.error(`The TXT record for ${d.domain} is not visible yet. DNS can take a while; try again.`);
            }
            await load();
        } catch (err) {
            app.checkApiError(err);
        }
    }

    function deleteDomain(d) {
        app.modals.confirm(`Release the claim on ${d.domain}?`, async () => {
            try {
                await app.pb.send(`/api/orgs/${getOrg().id}/sso/domains/${d.id}`, { method: "DELETE" });
                await load();
            } catch (err) {
                app.checkApiError(err);
            }
        });
    }

    const empty = (text) => t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, text)));

    const el = t.div(
        { className: "org-sso", id: "org_sso" },
        t.div(
            { className: "flex m-t-20 m-b-10" },
            t.h6({ className: "m-r-auto" }, "Single sign-on"),
            t.button(
                { type: "button", className: "btn sm", id: "sso_add_connection", hidden: () => !usable(), onclick: addConnection },
                t.i({ className: "ri-add-line", ariaHidden: true }),
                t.span({ className: "txt" }, "Add connection"),
            ),
        ),
        () => state.error ? t.div({ className: "alert danger m-b-10" }, state.error) : null,
        t.table(
            { className: "org-sso-connections-table" },
            t.thead(
                null,
                t.tr(
                    null,
                    t.th(null, "Type"),
                    t.th(null, "Name"),
                    t.th(null, "Enabled"),
                    t.th(null, "SSO required"),
                    t.th(null, "Domains"),
                    t.th({ className: "col-meta" }),
                ),
            ),
            t.tbody(null, () => {
                if (!state.connections.length) {
                    return empty(state.isLoading ? "Loading..." : "No connections. Nobody signs in through a provider until you add one.");
                }
                return state.connections.map((c) =>
                    t.tr(
                        { "html-data-sso-connection": c.id },
                        t.td(null, t.span({ className: "label sm" }, c.type)),
                        t.td(null, t.div({ className: "txt-bold" }, c.name), c.issuer ? t.div({ className: "txt-hint txt-sm txt-break" }, c.issuer) : null),
                        t.td(null, t.span({ className: `label sm ${c.enabled ? "success" : "warning"}` }, c.enabled ? "enabled" : "disabled")),
                        t.td(null, c.ssoRequired ? t.span({ className: "label sm danger" }, "required") : t.span({ className: "txt-hint" }, "no")),
                        t.td(null, state.domains.filter((d) => d.connection == c.id).map((d) => d.domain).join(", ") || t.span({ className: "txt-hint" }, "none")),
                        t.td(
                            { className: "col-meta" },
                            t.div(
                                { className: "inline-flex gap-xs" },
                                btn(c.enabled ? "ri-pause-circle-line" : "ri-play-circle-line", c.enabled ? "Disable" : "Enable", () => setEnabled(c, !c.enabled)),
                                c.type == "oidc" ? btn("ri-plug-line", "Test provider", () => testConnection(c)) : null,
                                btn("ri-pencil-line", "Edit", () => editConnection(c)),
                                btn("ri-delete-bin-7-line", "Delete", () => deleteConnection(c), "danger"),
                            ),
                        ),
                    )
                );
            }),
        ),
        t.div(
            { className: "flex m-t-20 m-b-10" },
            t.h6({ className: "m-r-auto" }, "Domain claims"),
            t.button(
                { type: "button", className: "btn sm", id: "sso_add_domain", hidden: () => !usable(), onclick: addDomain },
                t.i({ className: "ri-global-line", ariaHidden: true }),
                t.span({ className: "txt" }, "Claim domain"),
            ),
        ),
        t.table(
            { className: "org-sso-domains-table" },
            t.thead(
                null,
                t.tr(
                    null,
                    t.th(null, "Domain"),
                    t.th(null, "Connection"),
                    t.th(null, "Status"),
                    t.th(null, "TXT record to publish"),
                    t.th(null, "Last checked"),
                    t.th({ className: "col-meta" }),
                ),
            ),
            t.tbody(null, () => {
                if (!state.domains.length) {
                    return empty("No domain claims. Only a verified claim routes anyone to a connection.");
                }
                return state.domains.map((d) =>
                    t.tr(
                        { "html-data-sso-domain": d.id },
                        t.td(null, d.domain),
                        t.td(null, connName(d.connection)),
                        t.td(null, t.span({ className: `label sm ${d.status == "verified" ? "success" : "warning"}` }, d.status)),
                        t.td(
                            null,
                            d.verification
                                ? t.div(
                                    { className: "inline-flex gap-xs" },
                                    t.code({ className: "txt-break" }, d.verification.value),
                                    app.components.copyButton(d.verification.value),
                                )
                                : t.span({ className: "txt-hint" }, "verified " + when(d.verifiedAt)),
                        ),
                        t.td(null, when(d.lastCheckedAt) || "never"),
                        t.td(
                            { className: "col-meta" },
                            t.div(
                                { className: "inline-flex gap-xs" },
                                d.status != "verified"
                                    ? t.button(
                                        { type: "button", className: "btn sm secondary", onclick: () => verifyDomain(d) },
                                        t.i({ className: "ri-refresh-line", ariaHidden: true }),
                                        t.span({ className: "txt" }, "Verify now"),
                                    )
                                    : null,
                                btn("ri-delete-bin-7-line", "Release", () => deleteDomain(d), "danger"),
                            ),
                        ),
                    )
                );
            }),
        ),
    );

    return { el, load };
}
