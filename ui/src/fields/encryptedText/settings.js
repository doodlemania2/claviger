// {
//     originalCollection: undefined,
//     collection: undefined,
//     field
//     get fieldIndex: int/-1,
//     get originalField: undefined
// }
export function settings(props) {
    const uniqueId = "f_" + app.utils.randomString();

    function numberInput(prop, label, tooltip, placeholder, min) {
        return t.div(
            { className: "col-sm-6" },
            t.div(
                { className: "field" },
                t.label(
                    { htmlFor: uniqueId + "." + prop },
                    t.span({ className: "txt" }, label),
                    t.i({
                        className: "ri-information-line link-hint",
                        ariaDescription: app.attrs.tooltip(tooltip),
                    }),
                ),
                t.input({
                    type: "number",
                    id: uniqueId + "." + prop,
                    name: () => `fields.${props.fieldIndex}.${prop}`,
                    step: 1,
                    min: min,
                    max: Number.MAX_SAFE_INTEGER,
                    placeholder: placeholder,
                    value: () => props.field[prop] || "",
                    oninput: (e) => {
                        // temp skip invalid numbers with leading 0 while typing
                        // (always normalized in onchange)
                        if (e.target.value?.length > 1 && e.target.value[0] == "0") {
                            return;
                        }
                        props.field[prop] = parseInt(e.target.value, 10);
                    },
                    onchange: (e) => {
                        props.field[prop] = null; // reset reactivity
                        props.field[prop] = parseInt(e.target.value, 10);
                    },
                }),
            ),
        );
    }

    return app.components.fieldSettings(props, {
        // encrypted values are never used as record labels
        showPresentable: false,
        content: () =>
            t.div(
                { className: "grid sm" },
                t.div(
                    { className: "col-sm-12" },
                    t.div(
                        { className: "alert warning" },
                        t.div(
                            { className: "content" },
                            "The value is stored encrypted, but the API returns it as plaintext to anyone allowed to read the field. ",
                            t.strong(null, "It cannot be filtered, sorted, indexed or used in API rules"),
                            ", and losing the key makes the data unrecoverable (",
                            t.code(null, "CLAVIGER_KEK"),
                            ").",
                        ),
                    ),
                ),
                numberInput(
                    "min",
                    "Min length",
                    "Clear the field or set it to 0 for no limit.",
                    "No min limit",
                    0,
                ),
                numberInput(
                    "max",
                    "Max length",
                    "Clear the field or set it to 0 to fallback to the default limit.",
                    "Default to max 5000 characters",
                    () => props.field.min || 0,
                ),
                t.div(
                    { className: "col-sm-12" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + ".help" }, "Help text"),
                        t.input({
                            type: "text",
                            id: uniqueId + ".help",
                            name: () => `fields.${props.fieldIndex}.help`,
                            value: () => props.field.help || "",
                            oninput: (e) => (props.field.help = e.target.value),
                        }),
                    ),
                ),
            ),
        footer: () => [
            t.div(
                { className: "field" },
                t.input({
                    className: "sm",
                    type: "checkbox",
                    id: uniqueId + ".required",
                    name: () => `fields.${props.fieldIndex}.required`,
                    checked: () => !!props.field.required,
                    onchange: (e) => (props.field.required = e.target.checked),
                }),
                t.label(
                    { htmlFor: uniqueId + ".required" },
                    t.span({ className: "txt" }, "Required"),
                    t.small({ className: "txt-hint" }, "(!='')"),
                ),
            ),
        ],
    });
}
