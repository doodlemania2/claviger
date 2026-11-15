// Fork-local (Claviger #88, 88-5): data requests, from GET /api/gdpr/exports (superuser). The route
// lists export requests today. Erasure requests (88-4) are rendered by the same table when the
// route returns them: the Type column and the state flag come from each row.
//
// TODO(88-4): erasure actions. Add them in ONE place, the `rowActions(r)` function below
// (confirm, cancel, run now). None exist yet on purpose: this branch has no erasure routes.

import { cell, failer, flag, gdprPage, iconButton } from "./gdprCommon";

const STATE_CLASS = { done: "success", partial: "warning", failed: "danger", expired: "", cancelled: "" };

export function pageGdprRequests() {
    const state = store({ items: [], subject: "", error: "", notice: "", isLoading: false });
    const fail = failer(state);

    async function load() {
        state.isLoading = true;
        try {
            const subject = state.subject.trim();
            const res = await app.pb.send("/api/gdpr/exports", { query: subject ? { subject } : {} });
            state.items = res.items;
            state.error = "";
        } catch (err) {
            state.items = [];
            fail(err);
        }
        state.isLoading = false;
    }

    function resultText(r) {
        if (r.error) return r.error;
        if (!r.result) return "";
        const n = (v) => Array.isArray(v) ? v.length : v && typeof v == "object" ? Object.keys(v).length : 0;
        const bits = [];
        if (r.result.categories) bits.push(`${n(r.result.categories)} categories`);
        if (n(r.result.skipped)) bits.push(`${n(r.result.skipped)} skipped`);
        if (n(r.result.errors)) bits.push(`${n(r.result.errors)} errors`);
        if (r.result.files?.included !== undefined) bits.push(`${r.result.files.included} files`);
        if (r.result.bytes) bits.push(`${r.result.bytes} bytes`);
        return bits.join(", ");
    }

    // TODO(88-4): erasure actions go here, and only here.
    function rowActions(r) {
        return [
            r.type == "export" && r.downloadable
                ? iconButton("ri-download-2-line", `Download export ${r.id}`, async () => {
                    try {
                        const token = await app.pb.files.getToken();
                        window.open(`${app.pb.baseURL}/api/gdpr/exports/${r.id}/download?token=${encodeURIComponent(token)}`, "_blank", "noopener");
                    } catch (err) {
                        fail(err);
                    }
                })
                : null,
        ];
    }

    return gdprPage(
        {
            pbEvent: "pageGdprRequests",
            title: "Data requests",
            state,
            idPrefix: "requests",
            onmount: load,
            actions: app.components.refreshButton({ onclick: load }),
        },
        t.div({ className: "txt-hint txt-sm m-b-10" }, "Requests for a person's data. The due date is when the request should be answered. A download is available once, until the file expires."),
        t.form(
            {
                className: "grid m-b-10",
                onsubmit: (e) => {
                    e.preventDefault();
                    load();
                },
            },
            t.div(
                { className: "col-lg-5" },
                t.div(
                    { className: "field" },
                    t.input({ id: "requests_subject", type: "text", value: () => state.subject, oninput: (e) => state.subject = e.target.value }),
                    t.label({ htmlFor: "requests_subject" }, "Show one person (user id)"),
                ),
            ),
            t.div({ className: "col-lg-3" }, t.button({ type: "submit", className: "btn secondary" }, t.span({ className: "txt" }, "Filter"))),
        ),
        t.table(
            { className: "data-requests-table" },
            t.thead(null, t.tr(null, t.th(null, "Type"), t.th(null, "State"), t.th(null, "Subject"), t.th(null, "Requested"), t.th(null, "Due"), t.th(null, "Completed"), t.th(null, "Result"), t.th({ className: "col-meta" }, "Actions"))),
            t.tbody(null, () => {
                if (!state.items.length) {
                    return t.tr(null, t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No requests.")));
                }
                return state.items.map((r) =>
                    t.tr(
                        { "html-data-request": r.id, "html-data-type": r.type, "html-data-state": r.state },
                        t.td(null, flag(r.type)),
                        t.td(null, flag(r.state, STATE_CLASS[r.state] ?? "warning")),
                        t.td(null, `${r.subjectCollection}/`, t.code(null, r.subjectId), r.selfService ? flag("self service") : null),
                        t.td(null, cell(r.created)),
                        t.td(null, cell(r.dueAt)),
                        t.td(null, cell(r.completedAt)),
                        t.td(null, resultText(r)),
                        t.td({ className: "col-meta" }, t.div({ className: "inline-flex gap-xs" }, rowActions(r))),
                    )
                );
            }),
        ),
    );
}
