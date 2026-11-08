// {
//     collection: undefined,
//     originalRecord: undefined,
//     record: undefined,
//     field: undefined,
// }
export function input(props) {
    const uniqueId = "encrypted_text_" + app.utils.randomString();

    return t.div(
        { className: "record-field-input field-type-encryptedText" },
        t.div(
            { className: "fields" },
            t.div(
                { className: "field" },
                t.label(
                    { htmlFor: uniqueId },
                    t.i({ ariaHidden: true, className: app.fieldTypes.encryptedText.icon }),
                    t.span({ className: "txt" }, () => props.field.name),
                ),
                t.div(
                    { className: "autoexpand-wrapper" },
                    t.textarea({
                        id: uniqueId,
                        rows: 1,
                        name: () => props.field.name,
                        required: () => props.field.required,
                        autocomplete: "off",
                        spellcheck: false,
                        value: () => props.record[props.field.name] || "",
                        oninput: (e) => (props.record[props.field.name] = e.target.value || ""),
                    }),
                    t.div(
                        { className: "input" },
                        () => props.record[props.field.name],
                        // the empty space is necessary to prevent jumpy behavior
                        " ",
                    ),
                ),
            ),
        ),
        t.div(
            { className: "field-help" },
            () => props.field.help || "",
            t.small(
                { className: "txt-hint" },
                () => (props.field.help ? " " : "") + "Stored encrypted in the database. Shown here in plaintext.",
            ),
        ),
    );
}
