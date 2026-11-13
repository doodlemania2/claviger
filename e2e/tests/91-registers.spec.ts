import { test, expect, state, api } from "../fixtures";

// Permanent registers (#87): the register API against a real binary.
//
// Finalizing and annotating need a passkey assertion made moments ago, and the
// account of these tests signs in with a password, so the steps that follow a
// finalize (immutability, annotations, the certificate page) are covered by
// the Go tests (apis/registers_test.go, plugins/jsvm/registers_test.go),
// which can mint a token with a fresh passkey. What this spec proves against
// the real server: the register is created and registered in the data map, a
// draft stays editable, the guard refuses the direct writes, the routes demand
// the step-up, and the certificate assets are served.

test.describe.configure({ mode: "serial" });

let orgId = "";
let userToken = "";
let draftId = "";

async function call(method: string, path: string, body?: unknown, token = state().token) {
    const res = await fetch(state().base + path, {
        method,
        headers: { Authorization: token, "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let json: any = {};
    try {
        json = text ? JSON.parse(text) : {};
    } catch {
        /* an HTML answer */
    }
    return { status: res.status, json, text };
}

test.beforeAll(async () => {
    const s = state();
    const org = await api("POST", "/api/orgs", { name: "E2E Registers", slug: "e2e-registers", owner: s.user.email });
    orgId = org.id;
    const auth = await api("POST", "/api/collections/users/auth-with-password", { identity: s.user.email, password: s.user.password }, "");
    userToken = auth.token;
});

test("Registers: create a sacramental register and find it in the data map", async () => {
    const out = await api("POST", "/api/registers", { template: "sacramental", name: "e2e_baptisms" });
    expect(out).toMatchObject({ collection: "e2e_baptisms", annotationCollection: "e2e_baptisms_annotations", chain: true, signers: 1 });

    const map = await api("GET", "/api/collections/_dataMap/records?filter=" + encodeURIComponent('collection ~ "e2e_baptisms"'));
    expect(map.items.map((r: any) => r.collection).sort()).toEqual(["e2e_baptisms", "e2e_baptisms_annotations"]);
    for (const r of map.items) expect(r.treatment).toBe("retain");

    // the same name again is refused
    expect((await call("POST", "/api/registers", { template: "sacramental", name: "e2e_baptisms" })).status).toBe(409);
});

test("Registers: a draft is editable and the finalize fields cannot be written directly", async () => {
    const rec = await api("POST", "/api/collections/e2e_baptisms/records", { org: orgId, sacrament: "baptism", surname: "Smith" });
    draftId = rec.id;
    const edited = await api("PATCH", `/api/collections/e2e_baptisms/records/${draftId}`, { givenNames: "Ann", restricted: true });
    expect(edited.givenNames).toBe("Ann");

    for (const forged of [{ finalizedAt: "2024-01-01 00:00:00.000Z" }, { hash: "0".repeat(64) }, { pendingBy: "someone" }]) {
        const res = await call("PATCH", `/api/collections/e2e_baptisms/records/${draftId}`, forged);
        expect(res.status, JSON.stringify(forged)).toBe(400);
    }

    const created = await call("POST", "/api/collections/e2e_baptisms/records", {
        org: orgId, sacrament: "baptism", surname: "Forged", finalizedAt: "2024-01-01 00:00:00.000Z",
    });
    expect(created.status).toBe(400);
});

test("Registers: finalizing and annotating demand a fresh passkey", async () => {
    const fin = await call("POST", `/api/registers/e2e_baptisms/${draftId}/finalize`, undefined, userToken);
    expect(fin.status).toBe(403);
    expect(JSON.stringify(fin.json)).toContain("passkey_step_up_required");

    const note = await call(
        "POST",
        `/api/registers/e2e_baptisms/${draftId}/annotations`,
        { kind: "correction", text: "x", reason: "y" },
        userToken,
    );
    expect(note.status).toBe(403);

    // a draft has no certificate
    expect((await call("GET", `/api/registers/e2e_baptisms/${draftId}/certificate`, undefined, userToken)).status).toBe(409);

    // the entry view works for staff and starts without annotations
    const view = await call("GET", `/api/registers/e2e_baptisms/${draftId}`, undefined, userToken);
    expect(view.status).toBe(200);
    expect(view.json).toMatchObject({ state: "draft", annotations: [] });

    expect((await call("GET", `/api/registers/e2e_baptisms/${draftId}`, undefined, "")).status).toBe(401);
});

test("Registers: the register settings are fixed once set up, and the print stylesheet is served", async () => {
    const rows = await api("GET", "/api/collections/_permanentCollections/records?filter=" + encodeURIComponent('collection = "e2e_baptisms"'));
    const cfg = rows.items[0];
    expect((await call("PATCH", `/api/collections/_permanentCollections/records/${cfg.id}`, { chain: false })).status).toBe(400);
    expect((await call("PATCH", `/api/collections/_permanentCollections/records/${cfg.id}`, { signers: 2 })).status).toBe(200);

    // the register cannot be deleted
    expect((await call("DELETE", "/api/collections/e2e_baptisms")).status).toBe(400);

    const css = await fetch(state().base + "/orgs/assets/certificate.css");
    expect(css.status).toBe(200);
    expect(await css.text()).toContain("@media print");
});
