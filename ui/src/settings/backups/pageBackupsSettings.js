import { settingsSidebar } from "../settingsSidebar";
import { backupsForm } from "./backupsForm";
import { backupsList } from "./backupsList";
import { backupUploadButton } from "./backupUploadButton";

export function pageBackupsSettings(route) {
    app.store.title = "Backups";

    const data = store({
        resetList: null,
        database: "",
    });

    // Claviger: zip backups do not contain a Postgres database
    async function loadDatabase() {
        try {
            const health = await app.pb.health.check({ requestKey: null });
            data.database = health?.data?.database || "";
        } catch (err) {
            console.warn("failed to load the database engine", err);
        }
    }

    function resetBackupsList() {
        data.resetList = Date.now();
    }

    return t.div(
        {
            pbEvent: "pageBackupsSettings",
            className: "page page-backups-settings",
            onmount: () => {
                loadDatabase();
            },
        },
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
                t.div(
                    {
                        className: "grid",
                    },
                    t.div(
                        {
                            hidden: () => data.database != "postgres",
                            className: "col-lg-12",
                        },
                        t.div(
                            { className: "alert warning" },
                            t.div(
                                { className: "content" },
                                t.p(
                                    {},
                                    t.strong({}, "The database is PostgreSQL, so zip backups do not contain it."),
                                ),
                                t.p(
                                    {},
                                    "A backup holds the storage files only (and says so in its manifest), and restoring one is not available. ",
                                    "Back the database up with ",
                                    t.code({}, "pg_dump"),
                                    " or your provider's point-in-time recovery.",
                                ),
                            ),
                        ),
                    ),
                    t.div(
                        { className: "col-lg-12" },
                        t.div(
                            { className: "flex gap-10 m-b-sm" },
                            t.div({ className: "txt-lg" }, "Backup and restore your Claviger data"),
                            app.components.refreshButton({
                                className: "btn sm transparent secondary circle tooltip-bottom",
                                onclick: resetBackupsList,
                            }),
                            backupUploadButton(resetBackupsList),
                        ),
                        backupsList({
                            reset: () => data.resetList,
                        }),
                    ),
                    t.div(
                        { className: "col-lg-12" },
                        backupsForm({
                            onsave: () => resetBackupsList(),
                        }),
                    ),
                ),
            ),
            t.footer({ className: "page-footer" }, app.components.credits()),
        ),
    );
}
