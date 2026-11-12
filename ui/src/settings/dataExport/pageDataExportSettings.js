// Fork-local (Claviger #88): personal data export limits.

import { settingsSidebar } from "../settingsSidebar";

const FIELDS = [
    {
        key: "maxFileMB",
        label: "Largest single file (MB)",
        help: "A file of a file field above this size is left out of the ZIP and listed in the manifest. 0 = default (100).",
        max: 1024,
    },
    {
        key: "maxTotalMB",
        label: "Total size of files (MB)",
        help: "Files beyond this total are left out of the ZIP and listed in the manifest. 0 = default (1024).",
        max: 10240,
    },
    {
        key: "retentionDays",
        label: "Download available for (days)",
        help: "How long a finished export can be downloaded before it is deleted. 0 = default (7).",
        max: 30,
    },
    {
        key: "selfRequestsPerHour",
        label: "Requests per person per hour",
        help: "How many exports a person can request for themselves per hour. 0 = default (3).",
        max: 1000,
    },
    {
        key: "adminRequestsPerHour",
        label: "Requests per superuser per hour",
        help: "How many exports a superuser can request on behalf of others per hour. 0 = default (60).",
        max: 10000,
    },
];

export function pageDataExportSettings() {
    app.store.title = "Data export";

    const data = store({
        isLoading: false,
        isSaving: false,
        formSettings: null,
        initSerialized: "null",
        get hasChanges() {
            return data.initSerialized != JSON.stringify(data.formSettings);
        },
    });

    loadSettings();

    async function loadSettings() {
        data.isLoading = true;

        try {
            init(await app.pb.settings.getAll());
            data.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
            }
        }
    }

    function init(settings = {}) {
        app.store.settings = JSON.parse(JSON.stringify(settings));

        const dataExport = {};
        for (const f of FIELDS) {
            dataExport[f.key] = settings?.dataExport?.[f.key] || 0;
        }

        data.formSettings = { dataExport };
        data.initSerialized = JSON.stringify(data.formSettings);
    }

    async function save() {
        if (data.isSaving || !data.hasChanges) {
            return;
        }

        data.isSaving = true;

        try {
            init(await app.pb.settings.update(JSON.parse(JSON.stringify(data.formSettings))));

            app.toasts.success("Successfully saved data export settings.");
        } catch (err) {
            app.checkApiError(err);
        }

        data.isSaving = false;
    }

    function reset() {
        data.formSettings = JSON.parse(data.initSerialized);
    }

    function numberField(f, c) {
        return t.div(
            { className: "col-lg-6" },
            t.div(
                { className: "field" },
                t.label({ htmlFor: "dataExport_" + f.key }, f.label),
                t.input({
                    id: "dataExport_" + f.key,
                    name: "dataExport_" + f.key,
                    type: "number",
                    min: 0,
                    max: f.max,
                    step: 1,
                    value: () => c[f.key],
                    oninput: (e) => (c[f.key] = parseInt(e.target.value, 10) || 0),
                }),
                t.div({ className: "field-help" }, f.help),
            ),
        );
    }

    return t.div(
        { pbEvent: "pageDataExportSettings", className: "page page-data-export-settings" },
        settingsSidebar(),
        t.div(
            { className: "page-content full-height" },
            t.header(
                { className: "page-header" },
                t.nav(
                    { className: "breadcrumbs" },
                    t.div({ className: "breadcrumb-item" }, "Settings"),
                    t.div({ className: "breadcrumb-item" }, () => app.store.title),
                ),
            ),
            t.div(
                { className: "wrapper m-b-base" },
                () => {
                    if (data.isLoading) {
                        return t.div({ className: "block txt-center" }, t.span({ className: "loader lg" }));
                    }

                    const c = data.formSettings.dataExport;

                    return t.form(
                        {
                            pbEvent: "dataExportSettingsForm",
                            className: "grid data-export-settings-form",
                            inert: () => data.isSaving,
                            onsubmit: (e) => {
                                e.preventDefault();
                                save();
                            },
                        },
                        t.div(
                            { className: "col-lg-12" },
                            t.p(
                                null,
                                "Limits of the personal data export (GDPR Articles 15 and 20). Changes apply to the next request or job. The deprecated environment variables ",
                                t.code(null, "CLAVIGER_EXPORT_MAX_FILE_MB"),
                                ", ",
                                t.code(null, "CLAVIGER_EXPORT_MAX_TOTAL_MB"),
                                " and ",
                                t.code(null, "CLAVIGER_EXPORT_RETENTION_DAYS"),
                                " still win over these values while they are set; see docs/data-export.md.",
                            ),
                        ),
                        FIELDS.map((f) => numberField(f, c)),
                        t.div({ className: "col-lg-12" }, t.hr()),
                        t.div(
                            { className: "col-lg-12" },
                            t.div(
                                { className: "flex" },
                                t.div({ className: "m-r-auto" }),
                                t.button(
                                    {
                                        hidden: () => !data.hasChanges,
                                        type: "button",
                                        className: "btn transparent secondary",
                                        onclick: reset,
                                    },
                                    t.span({ className: "txt" }, "Cancel"),
                                ),
                                t.button(
                                    {
                                        className: () => `btn expanded-lg ${data.isSaving ? "loading" : ""}`,
                                        disabled: () => !data.hasChanges || data.isSaving,
                                    },
                                    t.span({ className: "txt" }, "Save changes"),
                                ),
                            ),
                        ),
                    );
                },
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
