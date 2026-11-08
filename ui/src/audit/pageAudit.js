// Fork-local (Claviger #74): read-only audit log view.

const COLLECTION = "_auditLog";
const perPage = 50;

const actionLabels = { create: "success", update: "warning", delete: "danger" };

function actorLabel(entry) {
    if (entry.actorType == "system" || !entry.actorId) {
        return "system";
    }
    return `${entry.actorCollection}/${entry.actorId}`;
}

function buildFilter(f) {
    const parts = [];
    const eq = (field, value) => {
        value = (value || "").trim();
        if (value) {
            parts.push(`${field}="${value.replaceAll("\\", "\\\\").replaceAll("\"", "\\\"")}"`);
        }
    };

    eq("targetCollection", f.collection);
    eq("targetId", f.record);
    eq("actorId", f.actor);
    eq("action", f.action);

    return parts.join(" && ");
}

export function pageAudit(route) {
    app.store.title = "Audit log";

    const ENTRY_QUERY_KEY = "entryId";
    const q = (key) => route.query[key]?.[0] || "";

    const state = store({
        collection: q("collection"),
        record: q("record"),
        actor: q("actor"),
        action: q("action"),
        reset: null,
        entries: [],
        lastPage: 1,
        lastLoadCount: 0,
        isLoading: false,
        activeEntryId: q(ENTRY_QUERY_KEY),
        get canLoadMore() {
            return state.lastLoadCount >= perPage;
        },
    });

    let loadStartDate;

    async function load(reset = false) {
        state.isLoading = true;

        try {
            let page;
            if (reset) {
                page = 1;
                loadStartDate = new Date().toISOString().replace("T", " ");
            } else {
                page = state.lastPage + 1;
            }

            const filter = [buildFilter(state), `created <= "${loadStartDate}"`]
                .filter(Boolean)
                .map((f) => "(" + f + ")")
                .join("&&");

            const result = await app.pb.collection(COLLECTION).getList(page, perPage, {
                skipTotal: 1,
                sort: "-created,-id",
                requestKey: "audit_list",
                filter,
            });

            if (result.page == 1) {
                state.entries = [];
            }

            state.lastPage = result.page;
            state.lastLoadCount = result.items.length;
            state.entries = state.entries.concat(result.items);
            state.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                state.isLoading = false;
                app.checkApiError(err);
            }
        }
    }

    function filterInput(key, label, placeholder, listId) {
        return t.div(
            { className: "field audit-filter" },
            t.input({
                type: "text",
                id: "audit_filter_" + key,
                placeholder: placeholder,
                ariaLabel: label,
                value: () => state[key],
                // "change" fires on blur/enter, which keeps this from querying on every keystroke
                onchange: (e) => state[key] = e.target.value.trim(),
                onkeydown: (e) => {
                    if (e.key == "Enter") {
                        state[key] = e.target.value.trim();
                    }
                },
            }),
            t.label({ htmlFor: "audit_filter_" + key }, label),
        );
    }

    const watchers = [];

    return t.div(
        {
            pbEvent: "pageAudit",
            className: "page page-audit",
            onmount() {
                watchers.push(
                    watch(
                        () => [state.collection, state.record, state.actor, state.action, state.reset],
                        () => {
                            app.utils.replaceHashQueryParams({
                                collection: state.collection,
                                record: state.record,
                                actor: state.actor,
                                action: state.action,
                            });
                            load(true);
                        },
                    ),
                    watch(() => state.activeEntryId, () => {
                        app.utils.replaceHashQueryParams({ [ENTRY_QUERY_KEY]: state.activeEntryId });

                        if (state.activeEntryId) {
                            app.modals.close(null, true);
                            app.modals.openAuditEntryPreview(state.activeEntryId, {
                                onafterclose: () => {
                                    state.activeEntryId = "";
                                },
                            });
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
                t.nav({ className: "breadcrumbs" }, t.div(null, "Audit log")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({ onclick: () => state.reset = Date.now() }),
                ),
                t.div(
                    { className: "inline-flex gap-sm audit-filters" },
                    filterInput("collection", "Collection", "e.g. users"),
                    filterInput("record", "Record id", "record id"),
                    filterInput("actor", "Actor id", "actor id"),
                    t.div(
                        { className: "field audit-filter" },
                        t.select(
                            {
                                id: "audit_filter_action",
                                ariaLabel: "Action",
                                value: () => state.action,
                                onchange: (e) => state.action = e.target.value,
                            },
                            t.option({ value: "" }, "Any action"),
                            t.option({ value: "create" }, "create"),
                            t.option({ value: "update" }, "update"),
                            t.option({ value: "delete" }, "delete"),
                        ),
                        t.label({ htmlFor: "audit_filter_action" }, "Action"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                t.table(
                    { className: "audit-table" },
                    t.thead(
                        null,
                        t.tr(
                            null,
                            t.th({ className: "col-field-type-date col-field-name-created" }, "When"),
                            t.th(null, "Action"),
                            t.th(null, "Actor"),
                            t.th(null, "Record"),
                            t.th(null, "Changed fields"),
                            t.th({ className: "col-meta" }),
                        ),
                    ),
                    t.tbody(
                        null,
                        () => {
                            if (!state.entries.length) {
                                return t.tr(
                                    null,
                                    t.td(
                                        { colSpan: 99 },
                                        state.isLoading
                                            ? t.span({ className: "skeleton-loader" })
                                            : t.div(
                                                { className: "sticky-content txt-center txt-hint" },
                                                t.div({ className: "txt-bold" }, "No audit entries found."),
                                            ),
                                    ),
                                );
                            }

                            return state.entries.map((entry) =>
                                t.tr(
                                    {
                                        rid: entry.id,
                                        tabIndex: 0,
                                        role: "button",
                                        className: "handle",
                                        onclick: () => state.activeEntryId = entry.id,
                                        onkeypress: (e) => {
                                            if (e.key == "Enter" || e.key == " ") {
                                                e.preventDefault();
                                                state.activeEntryId = entry.id;
                                            }
                                        },
                                    },
                                    t.td(
                                        { className: "col-field-type-date col-field-name-created" },
                                        app.components.formattedDate({ value: () => entry.created, short: false }),
                                    ),
                                    t.td(
                                        null,
                                        t.span({ className: `label sm ${actionLabels[entry.action] || ""}` }, entry.action),
                                    ),
                                    t.td(null, actorLabel(entry)),
                                    t.td(
                                        null,
                                        t.span({ className: "txt-bold" }, entry.targetCollection),
                                        " / ",
                                        entry.targetId,
                                    ),
                                    t.td(
                                        null,
                                        app.utils.truncate((entry.changedFields || []).join(", "), 120),
                                    ),
                                    t.td({ className: "col-meta" }, t.i({ className: "ri-arrow-right-line", ariaHidden: true })),
                                )
                            );
                        },
                        t.tr(
                            { hidden: () => !state.canLoadMore },
                            t.td(
                                { colSpan: 99 },
                                t.button(
                                    {
                                        className: () => `btn lg secondary load-more-btn ${state.isLoading ? "transparent loading" : ""}`,
                                        disabled: () => state.isLoading,
                                        onclick: () => load(),
                                    },
                                    t.span({ className: "txt" }, "Load older"),
                                ),
                            ),
                        ),
                    ),
                ),
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
