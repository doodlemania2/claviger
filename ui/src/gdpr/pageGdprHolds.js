// Fork-local (Claviger #88, 88-5): legal holds (docs/retention.md). Placing and lifting a hold
// each need a passkey assertion made moments ago: the page asks for it when the server says so
// and then repeats the request. A lifted hold stays in the list as evidence.

import { cell, failer, flag, gdprPage, message, withStepUp } from "./gdprCommon";

export function pageGdprHolds() {
    const state = store({ items: [], activeOnly: false, error: "", notice: "", isLoading: false });
    const fail = failer(state);

    async function load() {
        state.isLoading = true;
        try {
            const res = await app.pb.send("/api/gdpr/holds", { query: state.activeOnly ? { active: 1 } : {} });
            state.items = res.items;
            state.error = "";
        } catch (err) {
            state.items = [];
            fail(err);
        }
        state.isLoading = false;
    }

    function place() {
        app.modals.openOrgForm({
            title: "Place a legal hold",
            submitLabel: "Place the hold",
            fields: [
                { id: "scope", label: "Scope", type: "select", options: [{ value: "subject", label: "A person" }, { value: "record", label: "One record" }], value: "subject", hint: "A hold on a person covers the rows the data map ties to them. A hold on a record covers that record." },
                { id: "subject", label: "Person (user id), for a person", value: "" },
                { id: "subjectCollection", label: "Person's collection (empty for the default)", value: "" },
                { id: "collection", label: "Collection, for one record", value: "" },
                { id: "recordId", label: "Record id, for one record", value: "" },
                { id: "reason", label: "Reason", type: "textarea", value: "", required: true },
                { id: "reference", label: "Reference (a case number, optional)", value: "" },
            ],
            onsubmit: async (v) => {
                const body = { scope: v.scope, reason: v.reason, reference: v.reference };
                if (v.scope == "subject") {
                    body.subject = v.subject.trim();
                    if (v.subjectCollection.trim()) body.subjectCollection = v.subjectCollection.trim();
                } else {
                    body.collection = v.collection.trim();
                    body.recordId = v.recordId.trim();
                }
                try {
                    await withStepUp(() => app.pb.send("/api/gdpr/holds", { method: "POST", body }));
                } catch (err) {
                    err.response = { ...(err.response || {}), message: message(err) };
                    throw err;
                }
                state.notice = "The hold was placed.";
            },
            onsave: load,
        });
    }

    function lift(h) {
        app.modals.confirm("Lift this hold? The rows it covers can then be changed or deleted again. The hold stays in the list.", async () => {
            try {
                await withStepUp(() => app.pb.send(`/api/gdpr/holds/${h.id}/lift`, { method: "POST" }));
                state.notice = "The hold was lifted.";
                state.error = "";
                await load();
            } catch (err) {
                fail(err);
            }
        });
    }

    function scopeText(h) {
        return h.scope == "subject"
            ? `person ${h.subjectCollection || "users"}/${h.subject}`
            : `record ${h.collection}/${h.recordId}`;
    }

    return gdprPage(
        {
            pbEvent: "pageGdprHolds",
            title: "Legal holds",
            state,
            idPrefix: "holds",
            onmount: load,
            actions: [
                app.components.refreshButton({ onclick: load }),
                t.button(
                    { type: "button", id: "hold_new", className: "btn expanded-lg", onclick: place },
                    t.i({ className: "ri-add-line", ariaHidden: true }),
                    t.span({ className: "txt" }, "Place a hold"),
                ),
            ],
        },
        t.div({ className: "txt-hint txt-sm m-b-10" }, "While a hold is active, the retention job skips the rows it covers and nothing can delete a covered record. Placing and lifting a hold ask for a passkey."),
        t.div(
            { className: "field m-b-10" },
            t.input({ type: "checkbox", id: "holds_active", className: "switch", checked: () => state.activeOnly, onchange: (e) => { state.activeOnly = e.target.checked; load(); } }),
            t.label({ htmlFor: "holds_active" }, "Only active holds"),
        ),
        t.table(
            { className: "legal-holds-table" },
            t.thead(null, t.tr(null, t.th(null, "Scope"), t.th(null, "Reason"), t.th(null, "Reference"), t.th(null, "Placed"), t.th(null, "Lifted"), t.th({ className: "col-meta" }, "Actions"))),
            t.tbody(null, () => {
                if (!state.items.length) {
                    return t.tr(null, t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No holds.")));
                }
                return state.items.map((h) =>
                    t.tr(
                        { "html-data-hold": h.id, "html-data-active": String(h.active) },
                        t.td(null, scopeText(h), h.active ? flag("active", "warning") : null),
                        t.td(null, h.reason),
                        t.td(null, cell(h.reference)),
                        t.td(null, cell(h.placedAt)),
                        t.td(null, h.active ? "" : cell(h.liftedAt)),
                        t.td(
                            { className: "col-meta" },
                            h.active
                                ? t.button({ type: "button", className: "btn sm secondary", ariaLabel: `Lift hold ${h.id}`, onclick: () => lift(h) }, t.span({ className: "txt" }, "Lift"))
                                : null,
                        ),
                    )
                );
            }),
        ),
    );
}
