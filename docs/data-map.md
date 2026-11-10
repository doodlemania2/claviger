# The data map

The data map is a list of every collection in the app and what it holds about people. It is the base for data subject export and erasure (#88): those tools can only be correct if every collection that holds personal data is on the list.

It lives in the `_dataMap` system collection. Only superusers can see it. There is one row per collection.

## What a row says

| Field | Meaning |
|---|---|
| `collection` | The collection name. |
| `treatment` | What happens to the rows when a person is erased (see below). |
| `subjectPaths` | The fields that link a row to the person it is about, for example `recordRef`, `user`, or `id` when the row is the person. Empty when the treatment is `none`. |
| `personalFields` | The fields that hold personal data. |
| `retainReason` | Why the rows are kept. Required for `retain`. |
| `ageOutBasis` | The date field after which the rows age out (optional). |
| `note` | Free text: what the export contains, or why a collection is not personal. |
| `builtin` | True for entries that ship with Claviger. |

Treatments:

- `erase`: exported on request, deleted on erasure.
- `anonymize`: exported on request, the personal fields are blanked on erasure.
- `retain`: exported on request, kept on erasure for the stated reason.
- `export`: exported on request, left alone on erasure.
- `none`: reviewed, and holds no personal data. This is different from "forgotten": the check below treats `none` as mapped.

Export covers the subject's own rows only. Other people's data is left out. How the export works, and what each built-in collection exports, is in [data-export.md](data-export.md).

## Built-in entries

Every system collection has a built-in entry, written by migration `1780001500_add_data_map.go`. They are read-only through the API: even a superuser gets a 403 on changing or deleting them. The list is in `core/data_map_builtin.go`.

The request log table `_logs` (auxiliary database) is not a collection and has no row. Its personal data (user id and IP address) goes away with the log retention setting `logs.maxDays`.

## Adding a system collection

A new system collection needs a data map entry in the same change. A test fails until it has one.

1. Add one line to `builtinDataMap` in `core/data_map_builtin.go`:

   ```go
   {Collection: CollectionNameFoo, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"user"}, Note: "Export: ..."},
   ```

   Use `Treatment: DataMapTreatmentNone` with a note when the collection holds no personal data.

2. If the migration that creates the collection is numbered after `1780001500`, the seed has already run on existing installations. In that migration, call this after saving the collection instead:

   ```go
   return core.RegisterDataMapEntry(txApp, core.DataMapEntry{...})
   ```

   It does nothing when `_dataMap` does not exist yet, and updates the built-in row when it does.

The test is `TestDataMapCoversSystemCollections` in `core/data_map_test.go`. It bootstraps a fresh app and fails with the name of any system collection that has no entry.

3. Add the export allowlist line in `core/data_export_fields.go` (see [data-export.md](data-export.md)): the columns that may leave the system when the person asks for their data. `TestDataExportBuiltinAllowlistIsComplete` fails until it exists.

## App collections

Superusers register their own collections as normal records of `_dataMap` (records API, or the admin UI once it is built). A registration is checked: the collection must exist, `retain` needs a reason, and any treatment except `none` needs a subject path.

To see what is still missing:

```
GET /api/data-map/unmapped        (superuser only)
```

It returns the stored collections (not views) with no entry, system collections first:

```json
{"totalItems": 1, "items": [{"name": "prayers", "type": "base", "system": false}]}
```

This is an API route and not a CLI command because it is read-only and the admin UI will call the same route.
