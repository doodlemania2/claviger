# Runbook: SCIM against Microsoft Entra ID and Okta

Status: **not yet run against a real identity provider.** The SCIM server is covered by Go tests that replay the
requests Entra and Okta are documented to send (`docs/scim.md`). This runbook is for the first real run. It needs a
Claviger instance reachable over public https and an Entra tenant (and, separately, an Okta org) you can create an
enterprise application in. Everything below uses placeholders: nothing here is a real host, tenant or token.

Placeholders: `<host>` the public host name of the test instance, `<tenant>` your Entra tenant, `<slug>` the test
organization's slug, `<token>` a SCIM token (`scim_<prefix>_<secret>`).

## 0. Prepare the Claviger side

1. A **throw-away instance** with a public https URL (a temporary ingress or a tunnel, see
   `docs/oidc-conformance.md`, "Public https instance"). SCIM does not need the OIDC provider, so
   `PB_ENCRYPTION_KEY` is optional here.
2. Settings, Application: the application URL is `https://<host>`.
3. Create the test organization (admin UI, Organizations, **New organization**, slug `<slug>`) with a human owner you
   control. SCIM never grants ownership, and the last owner cannot be removed.
4. Create a SCIM token (**Organizations**, select the org, **SCIM provisioning**, **New token**). Copy it now, it is shown
   once. Tenant URL: `https://<host>/scim/v2`.
5. Check the token before involving the IdP:

   ```sh
   curl -s -H "Authorization: Bearer <token>" https://<host>/scim/v2/ServiceProviderConfig
   curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer scim_bad_bad" https://<host>/scim/v2/Users   # 401
   ```

6. Settings of the org (SCIM settings): leave `defaultRole` `member`, map one test group to `viewer` in `groupRoles`
   (for example `"Claviger Test Viewers": "viewer"`), and decide on `deactivationBans` (test both values, see 2.4).
7. Create the mailboxes for the test users first: SCIM requires `userName` to be an email address. Use test users whose
   addresses you control; a new account is created verified with a random password nobody knows.

## 1. Microsoft Entra ID

### 1.1 Configure provisioning

1. Entra admin center, Enterprise applications, **New application**, **Create your own application**, "Integrate any other
   application you don't find in the gallery (Non-gallery)".
2. Provisioning, **New configuration** (or Get started): Provisioning mode **Automatic**.
3. Tenant URL `https://<host>/scim/v2`, Secret Token `<token>`, **Test Connection**.
   - Expected: success. Entra queries a user that does not exist by a random GUID and expects `200` with an empty
     ListResponse. Failure with `401` means the token; with a timeout, the host is not reachable from Entra.
4. Mappings, Provision Microsoft Entra ID Users: keep these and delete the rest (see `docs/scim.md`):
   `userPrincipalName` (or `mail`) to `userName` (matching attribute, precedence 1), the `Switch(...)` expression to
   `active`, `displayName`, `givenName`/`surname` to `name.givenName`/`name.familyName`, `objectId` to `externalId`.
5. Mappings, Provision Microsoft Entra ID Groups: `displayName`, `members`, `objectId` to `externalId`.
6. Settings: Scope **Sync only assigned users and groups**. Do **not** switch provisioning on yet.

### 1.2 The SCIM validator (before real users)

The Microsoft Entra SCIM Validator checks the endpoint against what the provisioning service sends. Run it first.

1. Open <https://scimvalidator.microsoft.com/> (confirm the address in Microsoft Learn, "Tutorial: Validate a SCIM
   endpoint", if it has moved).
2. Choose **Discover schema** (Claviger serves `/Schemas`), enter the SCIM endpoint `https://<host>/scim/v2` and the
   token. Pick `userName` as the joining property for users and `displayName` for groups. Tick **Enable Group Tests**.
   Use an expression such as `{%generateRandomString 6%}@example.org` for `userName`, so each run creates a new email.
3. **Test Schema**, then open "show details".

Expected results, per validator test (the names come from the validator tutorial; verify against the current page):

| Validator test | Expected | Notes |
|---|---|---|
| Create New User | pass | `POST /Users` returns 201 with an `id`; the filter lookup finds it. The user is created in the org with `source = scim` |
| Create Duplicate User | pass | second `POST` is `409` with `scimType: uniqueness` |
| Add Attributes | pass | `PATCH add` of `name.*`, `displayName`, `externalId` |
| Replace User Attributes | pass | `PATCH replace` |
| Update Joining Property | pass | changing `userName` works only for accounts this org's SCIM created and that belong to no other org; otherwise `400 mutability` is the designed answer (data decision 3 in `docs/scim.md`), so a failure here on a **linked** account is expected |
| Update Active Attribute to False | pass | `active:false` suspends the membership; `GET` shows `active: false` |
| Create New Group / Duplicate Group | pass | group names become tags; duplicate is `409` |
| Update Group Attributes | pass | `displayName` rename renames the tag |
| Group member add (PATCH `members`) | pass | the user must exist in the org |
| Delete User / Group (cleanup) | pass | `DELETE /Users/{id}` removes the **membership**, not the account |

Known validator limitations (from the tutorial): soft deletes are not tested, a random time-zone value can fail systems
that validate it, and "patch user remove attributes" may try to remove required attributes. Record those as
"validator limitation", not as failures.

Clean up after each validator run: the users and groups it created stay as members and tags. Delete them in the org page
(or `DELETE` them with the token) before the next run.

### 1.3 Real provisioning

1. Create two test users and two groups in Entra (one group is the mapped `Claviger Test Viewers`). Assign them to the
   application.
2. Provisioning, **Provision on demand**, pick a user, **Provision**. Then start provisioning and wait for the first
   cycle (about 40 minutes), or keep using on-demand.

Scenarios and expected results:

| # | Action | Expected in Claviger |
|---|---|---|
| E1 | On-demand provision of a new user | Account created (verified, random password), membership `source: scim`, role `member`; `externalId` set |
| E2 | Provision a user whose email already has a **verified** account | Linked: membership created, the account's password and profile unchanged |
| E3 | Provision a user whose email has an **unverified** account | Entra logs a `409`; the account is untouched; verify it, run again, it links |
| E4 | Change `displayName` in Entra | Name updated (accounts SCIM created, in one org only) |
| E5 | Change the email of a user created by SCIM | `userName` change applied |
| E6 | Change the email of a linked account | `400 mutability` in the provisioning log; account unchanged |
| E7 | Add a user to `Claviger Test Viewers` | Tag `Claviger Test Viewers` and role `viewer` |
| E8 | Remove the user from that group | Tag removed; role falls back to the default role; membership stays |
| E9 | Unassign or disable a user (soft delete) | Membership `suspended`; user loses org access at once. With `deactivationBans` on and no other org: the account is banned (`_bans.createdBy = scim:<orgId>`, sessions revoked). Re-enable lifts only that ban |
| E10 | Delete the user in Entra (hard delete after 30 days, or on-demand) | `DELETE /Users/{id}`: membership removed, account kept |
| E11 | Try to deactivate the last owner | `409`; the owner stays |
| E12 | Revoke the token in Claviger, run on-demand | `401` in the Entra provisioning log; reissue a token and update the Secret Token |
| E13 | Audit | Every write shows in the audit log with `actorType: scim` and `meta.orgId` |

### 1.4 Clean up (Entra)

Stop provisioning, then delete the enterprise application (this ends the provisioning job). Delete the test users and
groups. In Claviger: revoke the token, delete the test members, archive and delete the org (archived orgs only, with the
slug as confirmation), delete the leftover test accounts, and tear down the instance and its public host name.

## 2. Okta

1. Applications, Browse App Catalog, "SCIM 2.0 Test App (Header Auth)" (or Create App Integration, SCIM 2.0).
2. Provisioning, Integration: base URL `https://<host>/scim/v2`; unique identifier field for users `userName`; enable
   Push New Users, Push Profile Updates, Push Groups. Authentication mode HTTP Header, Authorization `<token>` (bare or
   `Bearer <token>`). **Test Connector Configuration**.
3. To App: enable Create Users, Update User Attributes, Deactivate Users. Keep the default mappings; `userName` must be
   an email address.
4. Assign two test people; push one group.

| # | Action | Expected |
|---|---|---|
| O1 | Assign a new person | `GET /Users?filter=userName eq "..."&count=1` finds nothing, then `POST /Users` creates the member |
| O2 | Assign a person with a verified account | Linked, as E2 |
| O3 | Change the person's name | `PATCH` updates the name (SCIM-created accounts only) |
| O4 | Deactivate the person | `PATCH {"op":"replace","value":{"active":false}}`: membership suspended |
| O5 | Reactivate | `active:true`: membership active again |
| O6 | Push a group, add a member | Tag created, member tagged, mapped role applied |
| O7 | Remove a member from the pushed group | `members[value eq "<id>"]` remove: tag removed, membership stays |
| O8 | Unassign the person | Okta deactivates (O4) rather than deleting |
| O9 | `password` / `locale` sent by Okta | Ignored, never stored |

Clean up: deactivate the app integration, unassign and delete the test people, then the Claviger clean-up of 1.4.

## 3. Failure drills (do once)

- Wrong token: `401`, no data touched.
- 31 wrong tokens within a minute from one IP: `429` until the window ends.
- Archived org with a valid token: `403`.
- Provision a user while the instance is stopped: the IdP retries on its schedule; after restart the next cycle
  completes without duplicates (`externalId` is unique per org).

## Results template

```
SCIM real-world test
Date:                 
Tester:               
Claviger version/commit:      
Instance (public host, redacted):      
IdP: Entra ID [ ]  Okta [ ]     IdP build/version if shown:
Org settings: defaultRole=   groupRoles=   deactivationBans=on/off

Connection test:           pass [ ] fail [ ]   notes:
Entra SCIM validator:      passed __ / failed __ / limitation __   (attach "show details")
  failed tests and cause:
Scenarios E1..E13 / O1..O9 (pass / fail / n.a. + provisioning log excerpt id):
  E1  E2  E3  E4  E5  E6  E7  E8  E9  E10  E11  E12  E13
  O1  O2  O3  O4  O5  O6  O7  O8  O9
Failure drills (wrong token, 429, archived, outage):
Bugs found (issue links):
Clean-up done: IdP app [ ]  test users/groups [ ]  Claviger org/accounts [ ]  tokens revoked [ ]  host torn down [ ]
```
