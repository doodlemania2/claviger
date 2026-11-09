import { createHash, createPublicKey, randomBytes, verify } from "node:crypto";

export const b64url = (b: Buffer) => b.toString("base64url");

export function pkce() {
    const verifier = b64url(randomBytes(32));
    const challenge = b64url(createHash("sha256").update(verifier).digest());
    return { verifier, challenge };
}

export function authorizeUrl(base: string, p: Record<string, string>) {
    const u = new URL(base + "/oidc/authorize");
    for (const [k, v] of Object.entries(p)) u.searchParams.set(k, v);
    return u.toString();
}

export function decodeJwt(jwt: string) {
    const [h, p, s] = jwt.split(".");
    return {
        header: JSON.parse(Buffer.from(h, "base64url").toString()),
        claims: JSON.parse(Buffer.from(p, "base64url").toString()),
        signingInput: Buffer.from(`${h}.${p}`),
        signature: Buffer.from(s, "base64url"),
    };
}

/** Verifies an RS256/ES256 JWT against the provider's published key set. */
export async function verifyJwt(base: string, jwt: string): Promise<Record<string, any>> {
    const { header, claims, signingInput, signature } = decodeJwt(jwt);
    const jwks = (await (await fetch(base + "/oidc/keys")).json()) as { keys: any[] };
    const jwk = jwks.keys.find((k) => k.kid === header.kid);
    if (!jwk) throw new Error(`no key ${header.kid} in the JWKS`);
    const key = createPublicKey({ key: jwk, format: "jwk" });
    const ok =
        header.alg === "ES256"
            ? verify("sha256", signingInput, { key, dsaEncoding: "ieee-p1363" }, signature)
            : verify("sha256", signingInput, key, signature);
    if (!ok) throw new Error("bad signature");
    return claims;
}

export async function exchangeCode(base: string, o: { clientId: string; clientSecret: string; code: string; redirectUri: string; verifier: string }) {
    const res = await fetch(base + "/oidc/oauth/token", {
        method: "POST",
        headers: {
            "Content-Type": "application/x-www-form-urlencoded",
            Authorization: "Basic " + Buffer.from(`${o.clientId}:${o.clientSecret}`).toString("base64"),
        },
        body: new URLSearchParams({
            grant_type: "authorization_code",
            code: o.code,
            redirect_uri: o.redirectUri,
            code_verifier: o.verifier,
        }),
    });
    const body = await res.json();
    return { status: res.status, body: body as Record<string, any> };
}
