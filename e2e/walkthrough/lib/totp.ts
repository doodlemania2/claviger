import { createHmac } from "node:crypto";

function base32Decode(s: string): Buffer {
    const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
    let bits = "";
    for (const c of s.replace(/=+$/, "").toUpperCase()) {
        const v = alphabet.indexOf(c);
        if (v >= 0) bits += v.toString(2).padStart(5, "0");
    }
    const out: number[] = [];
    for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
    return Buffer.from(out);
}

/** RFC 6238, SHA-1, 6 digits, 30 s: what an authenticator app computes. */
export function totp(secret: string, at = Date.now(), step = 30): string {
    const counter = Buffer.alloc(8);
    counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / step)));
    const h = createHmac("sha1", base32Decode(secret)).update(counter).digest();
    const off = h[h.length - 1] & 0xf;
    return String((h.readUInt32BE(off) & 0x7fffffff) % 1_000_000).padStart(6, "0");
}
