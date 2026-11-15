// Fork-local (Claviger #88, 88-5): retention policies (docs/retention.md). A new policy is always
// created disabled and as a dry run. A preview counts what a policy would do and changes nothing.
// A policy that ends up enabled and live needs a passkey assertion made moments ago.

import { cell, failer, flag, gdprPage, iconButton, message, withStepUp } from "./gdprCommon";

const BASES = ["created", "updated", "ageOut"];
const UNITS = ["days", "months", "years"];
const ACTIONS = ["delete", "anonymize", "archive"];
const COUNTS = ["matched", "held", "unchanged", "failed", "affected"];

export function pageGdprRetention() {
    const state = store({ policies: [], preview: null, error: "", notice: "", isLoading: false, isPreviewing: false });
    const fail = failer(state);

    async function load() {
        state.isLoading = true;
        try {
            const res = await app.pb.send("/api/gdpr/retention/policies");
            state.policies = res.items;
            state.error = "";
        } catch (err) {
            state.policies = [];
            fail(err);
        }
        state.isLoading = false;
    }

    async function runPreview(policy) {
        state.isPreviewing = true;
        try {
            state.preview = await app.pb.send("/api/gdpr/retention/run", {
                method: "POST",
                body: { preview: true, ...(policy ? { policy: policy.id } : {}) },
            });
            state.error = "";
        } catch (err) {
            state.preview = null;
            fail(err);
        }
        state.isPreviewing = false;
    }

    function fields(p) {
        return [
            { id: "name", label: "Name", value: p?.name || "", required: true },
            { id: "collection", label: "Collection", value: p?.collection || "", required: true },
            { id: "basis", label: "Counts from (created, updated, ageOut or a date field)", value: p?.basis || "created", suggestions: BASES },
            { id: "after", label: "After", value: String(p?.after ?? 0) },
            { id: "unit", label: "Unit", type: "select", options: UNITS, value: p?.unit || "days" },
            { id: "action", label: "Action on a row past the period", type: "select", options: ACTIONS, value: p?.action || "delete" },
            { id: "filter", label: "Filter (optional, narrows the rows)", value: p?.filter || "" },
            ...(p
                ? [
                    { id: "enabled", label: "Enabled: the daily job counts this policy", type: "checkbox", value: !!p.enabled },
                    { id: "dryRun", label: "Dry run: count only, change nothing. Turning it off with the policy enabled asks for a passkey.", type: "checkbox", value: !!p.dryRun },
                ]
                : []),
        ];
    }

    function body(v, p) {
        const out = {
            name: v.name,
            collection: v.collection.trim(),
            basis: v.basis.trim(),
            after: parseInt(v.after || "0", 10),
            unit: v.unit,
            action: v.action,
            filter: v.filter,
        };
        if (p) {
            out.enabled = v.enabled;
            out.dryRun = v.dryRun;
        }
        return out;
    }

    async function send(fn) {
        try {
            return await fn();
        } catch (err) {
            err.response = { ...(err.response || {}), message: message(err) };
            throw err;
        }
    }

    function create() {
        app.modals.openOrgForm({
            title: "New retention policy",
            submitLabel: "Create",
            fields: fields(null),
            onsubmit: (v) =>
                send(async () => {
                    await app.pb.send("/api/gdpr/retention/policies", { method: "POST", body: body(v, null) });
                    state.notice = "The policy was created, disabled and as a dry run.";
                }),
            onsave: load,
        });
    }

    function edit(p) {
        app.modals.openOrgForm({
            title: `Edit ${p.name}`,
            submitLabel: "Save",
            fields: fields(p),
            onsubmit: (v) =>
                send(async () => {
                    await withStepUp(() => app.pb.send(`/api/gdpr/retention/policies/${p.id}`, { method: "PATCH", body: body(v, p) }));
                    state.notice = `${p.name} was saved.`;
                }),
            onsave: load,
        });
    }

    function remove(p) {
        app.modals.confirm(`Delete the policy ${p.name}? Rows it already changed stay changed.`, async () => {
            try {
                await app.pb.send(`/api/gdpr/retention/policies/${p.id}`, { method: "DELETE" });
                state.notice = `${p.name} was deleted.`;
                await load();
            } catch (err) {
                fail(err);
            }
        });
    }

    function countCells(c) {
        return COUNTS.map((k) => t.td(null, c && c[k] !== undefined ? String(c[k]) : ""));
    }

    function previewView() {
        const pv = state.preview;
        if (!pv) return null;
        const name = (id) => state.policies.find((p) => p.id == id)?.name || id;
        return t.div(
            { id: "retention_preview", className: "m-t-20" },
            t.h6({ className: "m-b-10" }, "Preview (nothing was changed)"),
            t.table(
                { className: "retention-preview-table" },
                t.thead(null, t.tr(null, t.th(null, "Policy"), t.th(null, "Collection"), t.th(null, "Action"), ...COUNTS.map((k) => t.th(null, k)))),
                t.tbody(
                    null,
                    pv.policies?.length
                        ? pv.policies.map((r) =>
                            t.tr(
                                { "html-data-policy": r.policyId },
                                t.td(null, name(r.policyId)),
                                t.td(null, r.collection),
                                t.td(null, r.action),
                                ...countCells(r),
                            )
                        )
                        : t.tr(null, t.td({ colSpan: 99 }, t.div({ className: "txt-center txt-hint" }, "No policy was previewed. Only enabled policies are included in a preview of all.")))
                ),
            ),
        );
    }

    function lastRun(p) {
        if (!p.lastRunAt && !p.runStatus) return t.span({ className: "txt-hint" }, "never");
        const c = p.lastResult || p.runCounts;
        const counts = c ? COUNTS.filter((k) => c[k] !== undefined).map((k) => `${k} ${c[k]}`).join(", ") : "";
        return t.div(
            null,
            p.runStatus ? flag(p.runStatus, p.runStatus == "running" ? "warning" : "") : null,
            t.div({ className: "txt-hint txt-sm" }, cell(p.lastRunAt)),
            counts ? t.div({ className: "txt-sm" }, counts) : null,
        );
    }

    return gdprPage(
        {
            pbEvent: "pageGdprRetention",
            title: "Retention policies",
            state,
            idPrefix: "retention",
            onmount: load,
            actions: [
                app.components.refreshButton({ onclick: load }),
                t.button(
                    { type: "button", id: "retention_preview_all", className: () => `btn secondary ${state.isPreviewing ? "loading" : ""}`, onclick: () => runPreview(null) },
                    t.span({ className: "txt" }, "Preview all enabled"),
                ),
                t.button(
                    { type: "button", id: "retention_new", className: "btn expanded-lg", onclick: create },
                    t.i({ className: "ri-add-line", ariaHidden: true }),
                    t.span({ className: "txt" }, "New policy"),
                ),
            ],
        },
        t.div({ className: "txt-hint txt-sm m-b-10" }, "A policy changes rows of one collection once they are older than its period. New policies start disabled and as a dry run. A preview counts what would happen and changes nothing. Rows under a legal hold are skipped."),
        t.table(
            { className: "retention-policies-table" },
            t.thead(null, t.tr(null, t.th(null, "Name"), t.th(null, "Collection"), t.th(null, "Rule"), t.th(null, "State"), t.th(null, "Last run"), t.th({ className: "col-meta" }, "Actions"))),
            t.tbody(null, () => {
                if (!state.policies.length) {
                    return t.tr(null, t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No policies.")));
                }
                return state.policies.map((p) =>
                    t.tr(
                        { "html-data-policy": p.id, "html-data-name": p.name },
                        t.td(null, t.span({ className: "txt-bold" }, p.name)),
                        t.td(null, t.code(null, p.collection)),
                        t.td(null, `${p.action} ${p.after} ${p.unit} after ${p.basis}`, p.filter ? t.div({ className: "txt-hint txt-sm" }, p.filter) : null),
                        t.td(null, flag(p.enabled ? "enabled" : "disabled", p.enabled ? "success" : ""), flag(p.dryRun ? "dry run" : "live", p.dryRun ? "" : "warning")),
                        t.td(null, lastRun(p)),
                        t.td(
                            { className: "col-meta" },
                            t.div(
                                { className: "inline-flex gap-xs" },
                                t.button({ type: "button", className: "btn sm secondary", ariaLabel: `Preview ${p.name}`, onclick: () => runPreview(p) }, t.span({ className: "txt" }, "Preview")),
                                iconButton("ri-pencil-line", `Edit ${p.name}`, () => edit(p)),
                                iconButton("ri-delete-bin-7-line", `Delete ${p.name}`, () => remove(p), "danger"),
                            ),
                        ),
                    )
                );
            }),
        ),
        () => previewView(),
    );
}
