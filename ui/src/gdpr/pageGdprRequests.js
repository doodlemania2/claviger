// Fork-local (Claviger #88, 88-5): data requests. Export requests come from GET /api/gdpr/exports and
// erasure requests (88-4) from GET /api/gdpr/erasures; one table shows both with their type and
// state. A superuser can view an erasure with its result, cancel one that has not started, start
// an erasure for a person (a fresh passkey is asked for), and set the grace period.

import { cell, failer, flag, gdprPage, iconButton, message, withStepUp } from "./gdprCommon";

const STATE_CLASS = { done: "success", partial: "warning", failed: "danger", expired: "", cancelled: "" };
const CANCELLABLE = ["awaiting_confirmation", "grace"];

export function pageGdprRequests() {
    const state = store({
        items: [],
        subject: "",
        error: "",
        notice: "",
        isLoading: false,
        view: null,
        // start an erasure
        eraseId: "",
        eraseCollection: "",
        person: null,
        identityCheck: "",
        requireConfirmation: false,
        graceDays: "",
        typed: "",
        isErasing: false,
        // setting
        settingGrace: "",
        settingLoaded: false,
    });
    const fail = failer(state);

    async function load() {
        state.isLoading = true;
        try {
            const query = state.subject.trim() ? { subject: state.subject.trim() } : {};
            const [exp, era] = await Promise.all([
                app.pb.send("/api/gdpr/exports", { query }),
                app.pb.send("/api/gdpr/erasures", { query }),
            ]);
            state.items = [...exp.items, ...era.items].sort((a, b) => String(b.created).localeCompare(String(a.created)));
            state.error = "";
        } catch (err) {
            state.items = [];
            fail(err);
        }
        state.isLoading = false;
    }

    async function loadSetting() {
        try {
            const s = await app.pb.settings.getAll();
            const g = s?.erasure?.graceDays;
            state.settingGrace = g === null || g === undefined ? "" : String(g);
            state.settingLoaded = true;
        } catch (err) {
            fail(err);
        }
    }

    async function saveSetting() {
        const raw = state.settingGrace.trim();
        const n = raw === "" ? null : parseInt(raw, 10);
        if (n !== null && (isNaN(n) || n < 0 || n > 30)) {
            state.error = "The grace period is from 0 to 30 days, or empty for the default (14).";
            return;
        }
        try {
            await app.pb.settings.update({ erasure: { graceDays: n } });
            state.error = "";
            state.notice = "The grace period was saved.";
        } catch (err) {
            fail(err);
        }
    }

    function resultText(r) {
        if (r.error) return r.error;
        if (!r.result) return "";
        const n = (v) => Array.isArray(v) ? v.length : v && typeof v == "object" ? Object.keys(v).length : 0;
        const bits = [];
        if (r.type == "erase") {
            const sum = (m) => Object.values(m || {}).reduce((a, b) => a + b, 0);
            bits.push(`${sum(r.result.erased)} erased`, `${sum(r.result.anonymized)} anonymized`);
            if (n(r.result.retained)) bits.push(`${n(r.result.retained)} kept`);
            if (r.result.refused) bits.push(`refused: ${r.result.refused}`);
            return bits.join(", ");
        }
        if (r.result.categories) bits.push(`${n(r.result.categories)} categories`);
        if (n(r.result.skipped)) bits.push(`${n(r.result.skipped)} skipped`);
        if (n(r.result.errors)) bits.push(`${n(r.result.errors)} errors`);
        if (r.result.files?.included !== undefined) bits.push(`${r.result.files.included} files`);
        if (r.result.bytes) bits.push(`${r.result.bytes} bytes`);
        return bits.join(", ");
    }

    async function openErasure(r) {
        try {
            state.view = await app.pb.send(`/api/gdpr/erasures/${r.id}`);
            state.error = "";
        } catch (err) {
            fail(err);
        }
    }

    function cancelErasure(r) {
        app.modals.confirm("Cancel this erasure? The account is unlocked and nothing is erased.", async () => {
            try {
                await withStepUp(() => app.pb.send(`/api/gdpr/erasures/${r.id}/cancel`, { method: "POST" }));
                state.notice = "The erasure was cancelled.";
                state.error = "";
                state.view = null;
                await load();
            } catch (err) {
                state.error = message(err);
            }
        });
    }

    function rowActions(r) {
        if (r.type == "erase") {
            return [
                t.button({ type: "button", className: "btn sm secondary", ariaLabel: `View erasure ${r.id}`, onclick: () => openErasure(r) }, t.span({ className: "txt" }, "View")),
                CANCELLABLE.includes(r.state)
                    ? t.button({ type: "button", className: "btn sm warning", ariaLabel: `Cancel erasure ${r.id}`, onclick: () => cancelErasure(r) }, t.span({ className: "txt" }, "Cancel"))
                    : null,
            ];
        }
        return [
            r.downloadable
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

    function countList(m) {
        const e = Object.entries(m || {});
        return e.length ? e.map(([k, v]) => `${k}: ${v}`).join(", ") : "none";
    }

    function erasureView() {
        const v = state.view;
        if (!v) return null;
        const res = v.result;
        return t.div(
            { id: "erasure_view", className: "m-t-20" },
            t.div(
                { className: "inline-flex gap-sm m-b-10" },
                t.h6({ className: "m-b-0" }, `Erasure ${v.id}`),
                flag(v.state, STATE_CLASS[v.state] ?? "warning"),
                t.button({ type: "button", className: "btn sm secondary", onclick: () => state.view = null }, t.span({ className: "txt" }, "Close")),
            ),
            t.dl(
                { className: "register-facts" },
                t.dt(null, "Subject"), t.dd(null, `${v.subjectCollection}/${v.subjectId}`),
                t.dt(null, "Mode"), t.dd(null, cell(v.mode)),
                v.graceUntil ? [t.dt(null, "Grace period ends"), t.dd(null, v.graceUntil)] : [],
                v.confirmedAt ? [t.dt(null, "Confirmed"), t.dd(null, v.confirmedAt)] : [],
                v.cancelledAt ? [t.dt(null, "Cancelled"), t.dd(null, v.cancelledAt)] : [],
                v.completedAt ? [t.dt(null, "Completed"), t.dd(null, v.completedAt)] : [],
                v.identityCheck ? [t.dt(null, "Identity check"), t.dd(null, v.identityCheck)] : [],
                v.error ? [t.dt(null, "Error"), t.dd(null, v.error)] : [],
            ),
            res
                ? t.div(
                    null,
                    t.h6({ className: "m-t-10 m-b-10" }, "Result"),
                    t.dl(
                        { className: "register-facts" },
                        t.dt(null, "Erased"), t.dd(null, countList(res.erased)),
                        t.dt(null, "Anonymized"), t.dd(null, countList(res.anonymized)),
                        t.dt(null, "Pseudonymized"), t.dd(null, countList(res.pseudonymized)),
                        t.dt(null, "Notice to the person"), t.dd(null, cell(res.notice)),
                        res.refused ? [t.dt(null, "Refused"), t.dd(null, res.refused)] : [],
                    ),
                    t.h6({ className: "m-t-10 m-b-10" }, "Kept"),
                    (res.retained || []).length
                        ? t.table(
                            { className: "erasure-retained-table" },
                            t.thead(null, t.tr(null, t.th(null, "Collection"), t.th(null, "Rows"), t.th(null, "Code"), t.th(null, "Reason"))),
                            t.tbody(null, res.retained.map((k) => t.tr(null, t.td(null, t.code(null, k.collection)), t.td(null, String(k.count)), t.td(null, k.code), t.td(null, k.reason)))),
                        )
                        : t.div({ className: "txt-hint txt-sm" }, "Nothing was kept."),
                )
                : t.div({ className: "txt-hint txt-sm" }, "No result yet: the job has not run."),
        );
    }

    // ---- start an erasure ----

    async function lookup() {
        const id = state.eraseId.trim();
        state.person = null;
        state.typed = "";
        if (!id) return;
        const col = state.eraseCollection.trim() || "users";
        try {
            const rec = await app.pb.send(`/api/collections/${encodeURIComponent(col)}/records/${encodeURIComponent(id)}`);
            state.person = { id: rec.id, email: rec.email || "", name: rec.name || "" };
            state.error = "";
        } catch (err) {
            fail(err);
        }
    }

    const typedMatches = () => {
        const p = state.person;
        const v = state.typed.trim().toLowerCase();
        return !!p && !!v && (v == p.id.toLowerCase() || (!!p.email && v == p.email.toLowerCase()));
    };
    const canErase = () => typedMatches() && (state.requireConfirmation || state.identityCheck.trim() != "") && !state.isErasing;

    async function startErasure() {
        if (!canErase()) return;
        const body = { requireConfirmation: state.requireConfirmation };
        if (state.eraseCollection.trim()) body.collection = state.eraseCollection.trim();
        if (state.identityCheck.trim()) body.identityCheck = state.identityCheck.trim();
        if (state.graceDays.trim() !== "") body.graceDays = parseInt(state.graceDays, 10);
        state.isErasing = true;
        try {
            await withStepUp(() => app.pb.send(`/api/gdpr/subjects/${encodeURIComponent(state.person.id)}/erase`, { method: "POST", body }));
            state.notice = "The erasure was requested.";
            state.error = "";
            state.person = null;
            state.eraseId = "";
            state.typed = "";
            state.identityCheck = "";
            await load();
        } catch (err) {
            state.error = message(err);
        }
        state.isErasing = false;
    }

    const field = (id, label, key, extra = {}) =>
        t.div(
            { className: "field" },
            t.input({ id, type: "text", value: () => state[key], oninput: (e) => state[key] = e.target.value, ...extra }),
            t.label({ htmlFor: id }, label),
        );

    function erasePanel() {
        return t.details(
            { id: "erase_panel", className: "m-t-30" },
            t.summary({ className: "txt-bold m-b-10" }, "Start an erasure for a person"),
            t.div(
                { className: "alert warning m-b-10" },
                "What happens: the account is locked and the person is told. After the grace period the person's data is deleted or anonymized as the data map says. Permanent register entries are kept and the result lists what was kept and why. It cannot be undone after the grace period. A superuser account cannot be erased here.",
            ),
            t.form(
                {
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        startErasure();
                    },
                },
                t.div({ className: "col-lg-5" }, field("erase_id", "Person (record id)", "eraseId", { oninput: (e) => { state.eraseId = e.target.value; state.person = null; state.typed = ""; } })),
                t.div({ className: "col-lg-4" }, field("erase_collection", "Collection (empty for users)", "eraseCollection", { oninput: (e) => { state.eraseCollection = e.target.value; state.person = null; state.typed = ""; } })),
                t.div({ className: "col-lg-3" }, t.button({ type: "button", id: "erase_lookup", className: "btn secondary", onclick: lookup }, t.span({ className: "txt" }, "Look up"))),
                () => state.person
                    ? t.div({ className: "col-lg-12", id: "erase_person" }, `Found ${state.person.name ? state.person.name + ", " : ""}${state.person.email || "no email"} (${state.person.id}).`)
                    : null,
                t.div(
                    { className: "col-lg-12" },
                    t.div(
                        { className: "field" },
                        t.input({ type: "checkbox", id: "erase_confirm_email", className: "switch", checked: () => state.requireConfirmation, onchange: (e) => state.requireConfirmation = e.target.checked }),
                        t.label({ htmlFor: "erase_confirm_email" }, "Ask the person to confirm by email first"),
                        t.div({ className: "field-help" }, "Off: you check who the person is yourself and the identity check below is required."),
                    ),
                ),
                t.div({ className: "col-lg-12" }, field("erase_identity", "Identity check (how you checked who the person is; kept in the request)", "identityCheck")),
                t.div({ className: "col-lg-4" }, field("erase_grace", "Grace period in days (0 to 30, empty for the setting)", "graceDays")),
                () => state.person
                    ? t.div(
                        { className: "col-lg-8" },
                        field("erase_typed", `Type ${state.person.email || state.person.id} to confirm`, "typed", { autocomplete: "off" }),
                    )
                    : null,
                t.div(
                    { className: "col-lg-12" },
                    t.button(
                        { type: "submit", id: "erase_start", className: () => `btn danger ${state.isErasing ? "loading" : ""}`, disabled: () => !canErase() },
                        t.span({ className: "txt" }, "Start the erasure"),
                    ),
                ),
            ),
        );
    }

    function settingPanel() {
        return t.div(
            { className: "m-t-30" },
            t.h6({ className: "m-b-10" }, "Grace period"),
            t.form(
                {
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        saveSetting();
                    },
                },
                t.div({ className: "col-lg-5" }, field("erasure_grace_days", "Days between confirmation and the job (0 to 30, empty for 14)", "settingGrace")),
                t.div({ className: "col-lg-3" }, t.button({ type: "submit", id: "erasure_grace_save", className: "btn secondary" }, t.span({ className: "txt" }, "Save"))),
            ),
        );
    }

    return gdprPage(
        {
            pbEvent: "pageGdprRequests",
            title: "Data requests",
            state,
            idPrefix: "requests",
            onmount: () => { load(); loadSetting(); },
            actions: app.components.refreshButton({ onclick: load }),
        },
        t.div({ className: "txt-hint txt-sm m-b-10" }, "Requests for a person's data: exports and erasures. The due date is when the request should be answered. An export can be downloaded once, until the file expires. An erasure can be cancelled until its grace period ends."),
        t.form(
            {
                className: "grid m-b-10",
                onsubmit: (e) => {
                    e.preventDefault();
                    load();
                },
            },
            t.div({ className: "col-lg-5" }, field("requests_subject", "Show one person (user id)", "subject")),
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
                        t.td(null, flag(r.state, STATE_CLASS[r.state] ?? "warning"), r.graceUntil && r.state == "grace" ? t.div({ className: "txt-hint txt-sm" }, `until ${r.graceUntil}`) : null),
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
        () => erasureView(),
        erasePanel(),
        () => state.settingLoaded ? settingPanel() : null,
    );
}
