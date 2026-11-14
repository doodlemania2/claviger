// Fork-local (Claviger #87, 87-5): permanent registers, for superusers. A
// superuser reads: the registers, their entries across orgs, an entry with its
// marginal notes (annotations), the hash chain check, and may withdraw a
// pending finalization. Finalizing and annotating are for registrars (never a
// superuser) and need a passkey, so they live on the hosted page
// /orgs/registers/{slug}; this page says so.

const STATES = ["", "draft", "pending", "final"];

function message(err) {
    return err?.response?.message || err?.message || "Request failed.";
}

export function pageRegisters() {
    app.store.title = "Registers";

    const state = store({
        registers: [],
        current: "",
        filter: "",
        page: 1,
        list: null,
        entry: null,
        error: "",
        notice: "",
        isLoading: false,
        verify: null,
        isVerifying: false,
    });

    const failed = (err) => {
        if (err?.isAbort) {
            return;
        }
        if (err?.status == 401) {
            return app.checkApiError(err);
        }
        state.error = message(err);
    };

    const register = () => state.registers.find((r) => r.collection == state.current);

    async function loadRegisters() {
        try {
            const res = await app.pb.send("/api/registers");
            state.registers = res.items;
            if (!register() && state.registers.length) {
                state.current = state.registers[0].collection;
            }
            state.error = "";
        } catch (err) {
            state.registers = [];
            failed(err);
        }
    }

    async function loadEntries() {
        const reg = register();
        if (!reg) {
            state.list = null;
            return;
        }
        state.isLoading = true;
        try {
            state.list = await app.pb.send(`/api/registers/${reg.collection}`, {
                query: { page: state.page, perPage: 25, ...(state.filter ? { state: state.filter } : {}) },
            });
            state.error = "";
        } catch (err) {
            state.list = null;
            failed(err);
        }
        state.isLoading = false;
    }

    async function load() {
        await loadRegisters();
        await loadEntries();
        if (state.entry) {
            await openEntry(state.entry.id);
        }
    }

    async function openEntry(id) {
        try {
            state.entry = await app.pb.send(`/api/registers/${state.current}/${id}`);
            state.error = "";
        } catch (err) {
            state.entry = null;
            failed(err);
        }
    }

    async function runVerify() {
        state.isVerifying = true;
        try {
            state.verify = await app.pb.send("/api/registers/_verify");
            state.error = "";
        } catch (err) {
            state.verify = null;
            failed(err);
        }
        state.isVerifying = false;
    }

    function withdraw(entry) {
        app.modals.confirm(`Withdraw the request to finalize this entry? It goes back to a draft.`, async () => {
            try {
                await app.pb.send(`/api/registers/${state.current}/${entry.id}/finalize`, { method: "DELETE" });
                state.notice = "The request was withdrawn.";
                await load();
            } catch (err) {
                failed(err);
            }
        });
    }

    function newRegister() {
        app.modals.openOrgForm({
            title: "New register",
            submitLabel: "Create",
            fields: [
                { id: "name", label: "Collection name (letters, digits and _; cannot change)", required: true },
                { id: "signers", label: "Signers", type: "select", options: ["1", "2"], value: "1", hint: "With 2, a registrar puts an entry forward and a different registrar confirms it." },
                { id: "retainReason", label: "Why entries are kept when someone asks for erasure (the legal reason)", type: "textarea", value: "" },
            ],
            onsubmit: async (v) => {
                try {
                    const res = await app.pb.send("/api/registers", {
                        method: "POST",
                        body: { template: "sacramental", name: v.name, signers: parseInt(v.signers, 10), retainReason: v.retainReason },
                    });
                    state.current = res.collection;
                    state.page = 1;
                    state.notice = `Register ${res.collection} created.`;
                } catch (err) {
                    err.response = { ...(err.response || {}), message: message(err) };
                    throw err;
                }
            },
            onsave: load,
        });
    }

    function flag(text, cls = "") {
        return t.span({ className: `label sm ${cls}` }, text);
    }

    function stateFlag(s) {
        return flag(s, s == "final" ? "success" : s == "pending" ? "warning" : "");
    }

    function cell(v) {
        if (v === undefined || v === null || v === "") return "";
        return typeof v == "object" ? JSON.stringify(v) : String(v);
    }

    function columns() {
        return (register()?.fields || []).filter((f) => ["text", "select", "date", "number"].includes(f.type)).slice(0, 3);
    }

    function entriesTable() {
        const cols = columns();
        const items = state.list?.items || [];
        return t.table(
            { className: "register-entries-table" },
            t.thead(null, t.tr(null, t.th(null, "State"), t.th(null, "No."), ...cols.map((c) => t.th(null, c.name)), t.th(null, "Org"), t.th({ className: "col-meta" }, "Actions"))),
            t.tbody(null, () => {
                if (!items.length) {
                    return t.tr(null, t.td({ colSpan: 99 }, state.isLoading ? t.span({ className: "skeleton-loader" }) : t.div({ className: "txt-center txt-hint" }, "No entries.")));
                }
                return items.map((it) =>
                    t.tr(
                        { "html-data-entry": it.id, "html-data-state": it.state },
                        t.td(null, stateFlag(it.state), it.record.restricted ? flag("restricted") : null),
                        t.td(null, cell(it.record.seq)),
                        ...cols.map((c) => t.td(null, cell(it.record[c.name]))),
                        t.td(null, t.code(null, it.org)),
                        t.td(
                            { className: "col-meta" },
                            t.button(
                                { type: "button", className: "btn sm secondary", ariaLabel: `Open entry ${it.id}`, onclick: () => openEntry(it.id) },
                                t.span({ className: "txt" }, "Open"),
                            ),
                        ),
                    )
                );
            }),
        );
    }

    function fact(label, value) {
        if (value === undefined || value === null || value === "") return [];
        return [t.dt(null, label), t.dd(null, value)];
    }

    function entryView() {
        const e = state.entry;
        if (!e) {
            return t.div({ className: "txt-hint txt-sm" }, "Open an entry to see it with its marginal notes.");
        }
        const rec = e.record;
        const reg = register();
        const retracted = new Set(e.annotations.filter((a) => a.kind == "retraction" && a.supersedes).map((a) => a.supersedes));
        return t.div(
            { id: "register_entry", className: "m-t-20" },
            t.div(
                { className: "inline-flex gap-sm m-b-10" },
                t.h6({ className: "m-b-0" }, `Entry ${e.id}`),
                stateFlag(e.state),
                t.button({ type: "button", className: "btn sm secondary", onclick: () => state.entry = null }, t.span({ className: "txt" }, "Close")),
                e.state == "pending"
                    ? t.button({ type: "button", id: "register_withdraw", className: "btn sm warning", onclick: () => withdraw(e) }, t.span({ className: "txt" }, "Withdraw the request"))
                    : null,
            ),
            t.dl(
                { className: "register-facts" },
                ...(reg?.fields || []).flatMap((f) => fact(f.name, cell(rec[f.name]))),
                ...fact("Final since", rec[reg?.finalizeField]),
                ...fact("Entered by", e.finalizedByName),
                ...fact("Confirmed by", e.secondSignerName),
                ...fact("Put forward by", e.state == "pending" ? e.pendingByName : ""),
                ...fact("Register number", rec.seq ? String(rec.seq) : ""),
                ...fact("Chain hash", rec.hash ? t.code(null, rec.hash) : ""),
            ),
            t.h6({ className: "m-t-20 m-b-10" }, "Marginal notes"),
            e.annotations.length
                ? t.ol(
                    { className: "register-notes" },
                    e.annotations.map((a) =>
                        t.li(
                            { "html-data-note": a.id, "html-data-kind": a.kind, className: "m-b-10" },
                            t.div(
                                { className: "txt-hint txt-sm" },
                                `${a.created} - ${a.kind.replace("_", " ")} - ${a.authorName || a.author}`,
                                retracted.has(a.id) ? flag("retracted", "warning") : null,
                            ),
                            t.div({ style: retracted.has(a.id) ? "text-decoration: line-through" : "" }, a.text),
                            t.div({ className: "txt-hint txt-sm" }, `Reason: ${a.reason}`),
                        )
                    ),
                )
                : t.div({ className: "txt-hint txt-sm" }, "No marginal notes."),
            t.div({ className: "txt-hint txt-sm m-t-10" }, "Notes are written by registrars on the hosted page /orgs/registers/<org slug>. A note can never be changed or removed."),
        );
    }

    function verifyView() {
        const v = state.verify;
        if (!v) {
            return t.div({ className: "txt-hint txt-sm" }, "Recompute the hash chain of every register to see whether an entry was changed outside the application.");
        }
        return t.div(
            { id: "register_verify" },
            t.div({ className: `alert ${v.ok ? "success" : "danger"} m-b-10`, role: "status" }, v.ok ? "Every chain is intact." : "A chain is broken."),
            t.table(
                { className: "register-verify-table" },
                t.thead(null, t.tr(null, t.th(null, "Register"), t.th(null, "Final entries"), t.th(null, "Chain"), t.th(null, "Head"), t.th(null, "Problems"))),
                t.tbody(
                    null,
                    v.registers.map((r) =>
                        t.tr(
                            { "html-data-register": r.collection },
                            t.td(null, r.collection),
                            t.td(null, String(r.finalized)),
                            t.td(null, r.chain ? "on" : "off"),
                            t.td(null, r.headSeq ? t.span(null, `${r.headSeq} `, t.code(null, r.headHash)) : ""),
                            t.td(null, r.breaks.length ? r.breaks.map((b) => t.div({ className: "txt-danger" }, `#${b.seq} ${b.entry}: ${b.problem}`)) : "none"),
                        )
                    ),
                ),
            ),
        );
    }

    return t.div(
        {
            pbEvent: "pageRegisters",
            className: "page page-registers",
            onmount() {
                load();
            },
        },
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav({ className: "breadcrumbs" }, t.div(null, "Registers")),
                t.div(
                    { className: "inline-flex gap-sm" },
                    app.components.refreshButton({ onclick: load }),
                    t.button(
                        { type: "button", className: "btn expanded-lg", onclick: newRegister },
                        t.i({ className: "ri-add-line", ariaHidden: true }),
                        t.span({ className: "txt" }, "New register"),
                    ),
                ),
            ),
            t.div(
                { className: "page-table-wrapper" },
                () => state.error ? t.div({ className: "alert danger m-b-10", id: "registers_error", role: "alert" }, state.error) : null,
                () => state.notice ? t.div({ className: "alert success m-b-10", id: "registers_notice", role: "status" }, state.notice) : null,
                t.div({ className: "txt-hint txt-sm m-b-10" }, "Registrars finalize entries and write marginal notes, with a passkey, on the hosted page of their org. A superuser reads, checks the chain and can withdraw a pending finalization."),
                () => state.registers.length
                    ? t.div(
                        { className: "inline-flex gap-sm m-b-10" },
                        t.div(
                            { className: "field" },
                            t.select(
                                { id: "register_select", onchange: (e) => { state.current = e.target.value; state.page = 1; state.entry = null; loadEntries(); } },
                                state.registers.map((r) => t.option({ value: r.collection, selected: () => state.current == r.collection }, r.collection)),
                            ),
                            t.label({ htmlFor: "register_select" }, "Register"),
                        ),
                        t.div(
                            { className: "field" },
                            t.select(
                                { id: "register_state", onchange: (e) => { state.filter = e.target.value; state.page = 1; loadEntries(); } },
                                STATES.map((s) => t.option({ value: s, selected: () => state.filter == s }, s || "All")),
                            ),
                            t.label({ htmlFor: "register_state" }, "Show"),
                        ),
                        register()
                            ? flag(`${register().signers} signer${register().signers == 1 ? "" : "s"}`)
                            : null,
                        register()?.chain ? flag("hash chain") : null,
                    )
                    : t.div({ className: "txt-center txt-hint m-b-10" }, "No registers yet. Create one from the template."),
                () => state.registers.length ? entriesTable() : null,
                () => state.list && state.list.totalItems > state.list.perPage
                    ? t.div(
                        { className: "inline-flex gap-sm m-t-10" },
                        t.button({ type: "button", className: "btn sm secondary", disabled: state.page <= 1, onclick: () => { state.page -= 1; loadEntries(); } }, "Previous"),
                        t.span({ className: "txt-hint" }, `Page ${state.list.page} of ${Math.ceil(state.list.totalItems / state.list.perPage)}`),
                        t.button({ type: "button", className: "btn sm secondary", disabled: state.page >= Math.ceil(state.list.totalItems / state.list.perPage), onclick: () => { state.page += 1; loadEntries(); } }, "Next"),
                    )
                    : null,
                () => entryView(),
                t.div(
                    { className: "m-t-30" },
                    t.h6({ className: "m-b-10" }, "Hash chain"),
                    t.button(
                        { type: "button", id: "register_verify_btn", className: () => `btn secondary m-b-10 ${state.isVerifying ? "loading" : ""}`, onclick: runVerify },
                        t.span({ className: "txt" }, "Verify the chain"),
                    ),
                    () => verifyView(),
                ),
            ),
        ),
    );
}
