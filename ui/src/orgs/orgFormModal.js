// Fork-local (Claviger #79a): small form modal shared by the organizations page
// (new organization, add member, edit member).

window.app = window.app || {};
window.app.modals = window.app.modals || {};

// fields: [{ id, label, type: "text"|"select"|"checkbox"|"textarea", options: [] (strings or {value, label}),
//            suggestions: [] (text fields: datalist), value, hint, required }]
// settings: { title, submitLabel, fields, onsubmit(values) -> Promise, onsave }
window.app.modals.openOrgForm = function(settings) {
    const modal = orgFormModal(settings);
    document.body.appendChild(modal);
    app.modals.open(modal);
};

function orgFormModal(settings) {
    // Unique per modal: a modal that is still closing stays in the DOM until its close
    // animation ends, and a shared id made the next modal's button submit the old form
    // (a second "New organization" request, answered 409) when opened quickly.
    const formId = "org_form_" + app.utils.randomString();
    const values = store(Object.fromEntries(settings.fields.map((f) => [f.id, f.value ?? (f.type == "checkbox" ? false : "")])));
    const state = store({ isSaving: false, error: "" });

    async function submit() {
        state.isSaving = true;
        state.error = "";
        try {
            await settings.onsubmit({ ...values });
            settings.onsave?.();
            app.modals.close();
        } catch (err) {
            state.error = err?.response?.message || err?.message || "Request failed.";
        }
        state.isSaving = false;
    }

    function input(f) {
        const id = formId + "_" + f.id;
        if (f.type == "select") {
            return t.select(
                { id, onchange: (e) => values[f.id] = e.target.value },
                f.options.map((o) => {
                    const value = typeof o == "object" ? o.value : o;
                    const label = typeof o == "object" ? o.label : o;
                    return t.option({ value, selected: () => values[f.id] == value }, label);
                }),
            );
        }
        if (f.type == "checkbox") {
            return t.input({
                type: "checkbox",
                id,
                className: "switch",
                checked: () => values[f.id],
                onchange: (e) => values[f.id] = e.target.checked,
            });
        }
        if (f.type == "textarea") {
            // fork-local (#88, 88-2): the consent text
            return t.textarea({
                id,
                rows: 6,
                required: !!f.required,
                value: () => values[f.id],
                oninput: (e) => values[f.id] = e.target.value,
            });
        }
        const el = t.input({
            type: "text",
            id,
            required: !!f.required,
            disabled: !!f.disabled,
            placeholder: f.placeholder || "",
            "html-list": f.suggestions?.length ? id + "_list" : undefined,
            value: () => values[f.id],
            oninput: (e) => values[f.id] = e.target.value,
        });
        if (!f.suggestions?.length) {
            return el;
        }
        return [
            el,
            t.datalist({ id: id + "_list" }, f.suggestions.map((v) => t.option({ value: v }))),
        ];
    }

    return t.div(
        {
            pbEvent: "orgFormModal",
            className: "modal org-form-modal",
            onafterclose: (el) => el?.remove(),
        },
        t.header({ className: "modal-header" }, t.h5(null, settings.title)),
        t.div(
            { className: "modal-content" },
            t.form(
                {
                    id: formId,
                    className: "grid",
                    onsubmit: (e) => {
                        e.preventDefault();
                        submit();
                    },
                },
                settings.fields.map((f) =>
                    t.div(
                        { className: "field" },
                        input(f),
                        t.label({ htmlFor: formId + "_" + f.id }, f.label),
                        f.hint ? t.div({ className: "field-help" }, f.hint) : null,
                    )
                ),
                () => state.error ? t.div({ className: "alert danger" }, state.error) : null,
            ),
        ),
        t.footer(
            { className: "modal-footer" },
            t.button(
                { type: "button", className: "btn transparent", onclick: () => app.modals.close() },
                t.span({ className: "txt" }, "Cancel"),
            ),
            t.button(
                {
                    type: "button",
                    className: () => `btn expanded ${state.isSaving ? "loading" : ""}`,
                    disabled: () => state.isSaving,
                    onclick: () => document.getElementById(formId)?.requestSubmit(),
                },
                t.span({ className: "txt" }, settings.submitLabel || "Save"),
            ),
        ),
    );
}
