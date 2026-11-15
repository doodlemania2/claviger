// Fork-local (Claviger #88, 88-5): the data map (docs/data-map.md). Lists every entry of _dataMap,
// warns about collections that have none (GET /api/data-map/unmapped) and lets a superuser add,
// change and remove entries for their own collections. Built-in entries are read-only: the
// server answers 403 for them, so no buttons are shown.

import { cell, failer, flag, gdprPage, iconButton, message } from "./gdprCommon";

const TREATMENTS = ["erase", "anonymize", "retain", "export", "none"];
const TREATMENT_HELP = {
    erase: "exported on request, deleted on erasure",
    anonymize: "exported on request, personal fields blanked on erasure",
    retain: "exported on request, kept on erasure for the stated reason",
    export: "exported on request, left alone on erasure",
    none: "reviewed, holds no personal data",
};

const list = (v) => Array.isArray(v) ? v : [];
const split = (s) => String(s || "").split(",").map((x) => x.trim()).filter(Boolean);

export function pageGdprDataMap() {
    const state = store({ entries: [], unmapped: [], error: "", notice: "", isLoading: false });
    const fail = failer(state);

    async function load() {
        state.isLoading = true;
        try {
            const [map, unmapped] = await Promise.all([
                app.pb.send("/api/collections/_dataMap/records", { query: { perPage: 500, sort: "collection" } }),
                app.pb.send("/api/data-map/unmapped"),
            ]);
            state.entries = map.items;
            state.unmapped = unmapped.items;
            state.error = "";
        } catch (err) {
            fail(err);
        }
        state.isLoading = false;
    }

    function fields(e, collection = "") {
        return [
            { id: "collection", label: "Collection", value: e?.collection || collection, required: true, disabled: !!e, suggestions: state.unmapped.map((u) => u.name) },
            { id: "treatment", label: "Treatment on erasure", type: "select", options: TREATMENTS, value: e?.treatment || "none", hint: TREATMENTS.map((t) => `${t}: ${TREATMENT_HELP[t]}`).join(". ") + "." },
            { id: "subjectPaths", label: "Subject paths (fields that link a row to a person, comma separated; id when the row is the person)", value: list(e?.subjectPaths).join(", ") },
            { id: "personalFields", label: "Personal fields (comma separated)", value: list(e?.personalFields).join(", ") },
            { id: "retainReason", label: "Retain reason (required for retain)", type: "textarea", value: e?.retainReason || "" },
            { id: "ageOutBasis", label: "Age-out basis (date field, optional)", value: e?.ageOutBasis || "" },
            { id: "note", label: "Note", value: e?.note || "" },
        ];
    }

    function openForm(e, collection = "") {
        app.modals.openOrgForm({
            title: e ? `Edit ${e.collection}` : "Map a collection",
            submitLabel: e ? "Save" : "Add",
            fields: fields(e, collection),
            onsubmit: async (v) => {
                const body = {
                    treatment: v.treatment,
                    subjectPaths: split(v.subjectPaths),
                    personalFields: split(v.personalFields),
                    retainReason: v.retainReason,
                    ageOutBasis: v.ageOutBasis,
                    note: v.note,
                };
                try {
                    if (e) {
                        await app.pb.send(`/api/collections/_dataMap/records/${e.id}`, { method: "PATCH", body });
                    } else {
                        await app.pb.send("/api/collections/_dataMap/records", { method: "POST", body: { ...body, collection: v.collection.trim() } });
                    }
                } catch (err) {
                    err.response = { ...(err.response || {}), message: message(err) };
                    throw err;
                }
                state.notice = `${e ? e.collection : v.collection.trim()} saved.`;
            },
            onsave: load,
        });
    }

    function remove(e) {
        app.modals.confirm(`Remove the data map entry of ${e.collection}? The collection then shows as unmapped.`, async () => {
            try {
                await app.pb.send(`/api/collections/_dataMap/records/${e.id}`, { method: "DELETE" });
                state.notice = `${e.collection} removed from the data map.`;
                await load();
            } catch (err) {
                fail(err);
            }
        });
    }

    function unmappedView() {
        if (!state.unmapped.length) {
            return t.div({ id: "datamap_all_mapped", className: "txt-hint txt-sm m-b-10" }, "Every collection has a data map entry.");
        }
        return t.div(
            { id: "datamap_unmapped", className: "alert warning m-b-10", role: "status" },
            t.div({ className: "txt-bold m-b-5" }, `${state.unmapped.length} collection${state.unmapped.length == 1 ? " has" : "s have"} no data map entry`),
            t.div(
                { className: "inline-flex gap-sm", style: "flex-wrap: wrap" },
                state.unmapped.map((u) =>
                    t.button(
                        { type: "button", className: "btn sm secondary", "html-data-unmapped": u.name, onclick: () => openForm(null, u.name) },
                        t.span({ className: "txt" }, `${u.name}${u.system ? " (system)" : ""}`),
                    )
                ),
            ),
        );
    }

    return gdprPage(
        {
            pbEvent: "pageGdprDataMap",
            title: "Data map",
            state,
            idPrefix: "datamap",
            onmount: load,
            actions: [
                app.components.refreshButton({ onclick: load }),
                t.button(
                    { type: "button", id: "datamap_new", className: "btn expanded-lg", onclick: () => openForm(null) },
                    t.i({ className: "ri-add-line", ariaHidden: true }),
                    t.span({ className: "txt" }, "Map a collection"),
                ),
            ],
        },
        t.div({ className: "txt-hint txt-sm m-b-10" }, "The data map says which collections hold data about people and what a data export and an erasure do with them. Built-in entries cannot be changed."),
        () => unmappedView(),
        t.table(
            { className: "datamap-table" },
            t.thead(null, t.tr(null, t.th(null, "Collection"), t.th(null, "Treatment"), t.th(null, "Subject paths"), t.th(null, "Fields"), t.th(null, "Retain reason"), t.th(null, "Age-out basis"), t.th({ className: "col-meta" }, "Actions"))),
            t.tbody(null, () => {
                if (!state.entries.length) {
                    return t.tr(null, t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No entries.")));
                }
                return state.entries.map((e) =>
                    t.tr(
                        { "html-data-collection": e.collection, "html-data-treatment": e.treatment },
                        t.td(null, t.code(null, e.collection), e.builtin ? flag("built in") : null),
                        t.td(null, flag(e.treatment, e.treatment == "none" ? "" : e.treatment == "retain" ? "warning" : "success")),
                        t.td(null, list(e.subjectPaths).join(", ")),
                        t.td(null, list(e.personalFields).join(", ")),
                        t.td(null, cell(e.retainReason)),
                        t.td(null, cell(e.ageOutBasis)),
                        t.td(
                            { className: "col-meta" },
                            e.builtin
                                ? null
                                : t.div(
                                    { className: "inline-flex gap-xs" },
                                    iconButton("ri-pencil-line", `Edit ${e.collection}`, () => openForm(e)),
                                    iconButton("ri-delete-bin-7-line", `Remove ${e.collection}`, () => remove(e), "danger"),
                                ),
                        ),
                    )
                );
            }),
        ),
    );
}
