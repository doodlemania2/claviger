// Fork-local (Claviger #93): sessions panel and ban/unban for an auth record.

window.app = window.app || {};
window.app.modals = window.app.modals || {};

/**
 * Opens the sessions and ban panel of an auth record.
 *
 * @param {Object} record
 */
window.app.modals.openRecordSessions = function(record) {
    const modal = recordSessionsModal(record);

    document.body.appendChild(modal);

    app.modals.open(modal);
};

function recordSessionsModal(record) {
    const uniqueId = "sessions_" + app.utils.randomString();

    const data = store({
        isLoading: false,
        isBusy: false,
        sessions: [],
        ban: null,
        banReason: "",
        banUntil: "",
    });

    const query = { collection: record.collectionId || record.collectionName, record: record.id };

    async function load() {
        data.isLoading = true;

        try {
            const [sessions, bans] = await Promise.all([
                app.pb.send("/api/sessions", { query: { ...query, all: "true" }, requestKey: uniqueId + "_s" }),
                app.pb.send("/api/bans", { query, requestKey: uniqueId + "_b" }),
            ]);
            data.sessions = sessions.items || [];
            data.ban = (bans.items || [])[0] || null;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }

        data.isLoading = false;
    }

    async function send(path, options, successMessage) {
        if (data.isBusy) {
            return;
        }

        data.isBusy = true;

        try {
            await app.pb.send(path, options);
            app.toasts.success(successMessage);
            await load();
        } catch (err) {
            app.checkApiError(err);
        }

        data.isBusy = false;
    }

    function revokeOne(session) {
        return send("/api/sessions/" + encodeURIComponent(session.id), { method: "DELETE" }, "The session was revoked.");
    }

    function revokeAll() {
        app.modals.confirm(
            "Revoke every session of this account? Tokens issued before sessions were tracked keep working until they expire.",
            () =>
                send(
                    "/api/sessions/revoke-all",
                    { method: "POST", body: { ...query, reason: data.banReason } },
                    "The sessions were revoked.",
                ),
        );
    }

    function ban() {
        app.modals.confirm(
            "Ban this account? It is signed out everywhere at once and cannot sign in until the ban is lifted.",
            () =>
                send(
                    "/api/bans",
                    {
                        method: "POST",
                        body: {
                            ...query,
                            reason: data.banReason,
                            until: data.banUntil ? new Date(data.banUntil).toISOString().replace("T", " ") : "",
                        },
                    },
                    "The account was banned.",
                ),
        );
    }

    function unban() {
        return send("/api/bans/" + encodeURIComponent(data.ban.id), { method: "DELETE" }, "The ban was lifted.");
    }

    function sessionStatus(s) {
        if (s.revoked) {
            return t.span({ className: "label danger", title: s.revokedReason }, "Revoked");
        }
        if (!s.active) {
            return t.span({ className: "label warning" }, "Expired");
        }
        return t.span({ className: "label success" }, "Active");
    }

    return t.div(
        {
            pbEvent: "recordSessionsModal",
            className: "modal popup record-sessions-modal",
            onbeforeopen: () => {
                load();
            },
            onafterclose: (el) => {
                el?.remove();
            },
        },
        t.header(
            { className: "modal-header" },
            t.h6(null, "Sessions and ban: ", t.strong(null, () => record.email || record.id)),
        ),
        t.div(
            { className: "modal-content" },
            // ban state
            t.div(
                {
                    className: () => `alert ${data.ban ? "danger" : "info"} m-b-base`,
                },
                t.div(
                    { className: "content" },
                    () => {
                        if (!data.ban) {
                            return "This account is not banned.";
                        }
                        return [
                            t.strong(null, "Banned"),
                            data.ban.until ? " until " + app.utils.toLocalDatetime(data.ban.until) : " (no expiry)",
                            data.ban.reason ? ": " + data.ban.reason : "",
                        ];
                    },
                ),
            ),
            t.div(
                { className: "grid sm m-b-base", hidden: () => !!data.ban },
                t.div(
                    { className: "col-lg-6" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + "_reason" }, "Reason (visible to superusers only)"),
                        t.input({
                            id: uniqueId + "_reason",
                            type: "text",
                            maxLength: 255,
                            value: () => data.banReason,
                            oninput: (e) => (data.banReason = e.target.value),
                        }),
                    ),
                ),
                t.div(
                    { className: "col-lg-6" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + "_until" }, "Ban until (optional)"),
                        t.input({
                            id: uniqueId + "_until",
                            type: "datetime-local",
                            value: () => data.banUntil,
                            oninput: (e) => (data.banUntil = e.target.value),
                        }),
                    ),
                ),
            ),
            // sessions
            t.table(
                { className: "record-sessions-table" },
                t.thead(
                    null,
                    t.tr(
                        null,
                        t.th(null, "Device"),
                        t.th(null, "IP"),
                        t.th(null, "Method"),
                        t.th(null, "Last seen"),
                        t.th(null, "Status"),
                        t.th({ className: "col-meta" }),
                    ),
                ),
                t.tbody(
                    null,
                    () => {
                        if (!data.sessions.length) {
                            return t.tr(
                                null,
                                t.td(
                                    { colSpan: 99 },
                                    data.isLoading
                                        ? t.span({ className: "skeleton-loader" })
                                        : t.div({ className: "txt-center txt-hint" }, "No tracked sessions."),
                                ),
                            );
                        }

                        return data.sessions.map((s) =>
                            t.tr(
                                { rid: s.id },
                                t.td({ className: "txt-ellipsis", title: s.userAgent }, s.userAgent || "Unknown"),
                                t.td(null, s.ip),
                                t.td(null, s.authMethod),
                                t.td(null, s.lastSeen ? app.utils.toLocalDatetime(s.lastSeen) : ""),
                                t.td(null, sessionStatus(s)),
                                t.td(
                                    { className: "col-meta" },
                                    s.active
                                        ? t.button(
                                            {
                                                type: "button",
                                                className: "btn sm danger transparent",
                                                disabled: () => data.isBusy,
                                                onclick: () => revokeOne(s),
                                            },
                                            t.span({ className: "txt" }, "Revoke"),
                                        )
                                        : null,
                                ),
                            )
                        );
                    },
                ),
            ),
        ),
        t.footer(
            { className: "modal-footer" },
            t.button(
                {
                    type: "button",
                    className: "btn transparent m-r-auto",
                    onclick: (e) => app.modals.close(e.target.closest(".modal")),
                },
                t.span({ className: "txt" }, "Close"),
            ),
            t.button(
                {
                    type: "button",
                    className: "btn secondary",
                    disabled: () => data.isBusy || !data.sessions.some((s) => s.active),
                    onclick: () => revokeAll(),
                },
                t.span({ className: "txt" }, "Revoke all sessions"),
            ),
            () =>
                data.ban
                    ? t.button(
                        {
                            type: "button",
                            className: "btn",
                            disabled: () => data.isBusy,
                            onclick: () => unban(),
                        },
                        t.span({ className: "txt" }, "Unban"),
                    )
                    : t.button(
                        {
                            type: "button",
                            className: "btn danger",
                            disabled: () => data.isBusy,
                            onclick: () => ban(),
                        },
                        t.span({ className: "txt" }, "Ban account"),
                    ),
        ),
    );
}
