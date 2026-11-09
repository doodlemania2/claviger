import { createServer, type Server, type Socket } from "node:net";

export type Mail = { from: string; to: string[]; raw: string; text: string };

/** Quoted-printable decoder, enough for the mail bodies the server sends. */
function qp(s: string): string {
    return s.replace(/=\r?\n/g, "").replace(/=([0-9A-F]{2})/g, (_, h) => String.fromCharCode(parseInt(h, 16)));
}

/** A tiny SMTP sink: accepts everything on 127.0.0.1 and keeps the messages. */
export async function startSmtp(port: number): Promise<{ mails: Mail[]; close: () => Promise<void>; waitFor: (pred: (m: Mail) => boolean, ms?: number) => Promise<Mail> }> {
    const mails: Mail[] = [];
    const sockets = new Set<Socket>();

    const server: Server = createServer((sock) => {
        sockets.add(sock);
        sock.on("close", () => sockets.delete(sock));
        sock.on("error", () => {});
        let buf = "";
        let inData = false;
        let cur: Mail = { from: "", to: [], raw: "", text: "" };
        sock.write("220 sink ESMTP\r\n");
        sock.on("data", (d) => {
            buf += d.toString("latin1");
            for (;;) {
                if (inData) {
                    const end = buf.indexOf("\r\n.\r\n");
                    if (end < 0) return;
                    cur.raw = buf.slice(0, end);
                    cur.text = qp(cur.raw);
                    buf = buf.slice(end + 5);
                    inData = false;
                    mails.push(cur);
                    cur = { from: "", to: [], raw: "", text: "" };
                    sock.write("250 queued\r\n");
                    continue;
                }
                const nl = buf.indexOf("\r\n");
                if (nl < 0) return;
                const line = buf.slice(0, nl);
                buf = buf.slice(nl + 2);
                const cmd = line.slice(0, 4).toUpperCase();
                if (cmd === "EHLO" || cmd === "HELO") sock.write("250-sink\r\n250 8BITMIME\r\n");
                else if (cmd === "MAIL") (cur.from = line.slice(10).replace(/[<>]/g, "")), sock.write("250 ok\r\n");
                else if (cmd === "RCPT") cur.to.push(line.slice(8).replace(/[<>]/g, "")), sock.write("250 ok\r\n");
                else if (cmd === "DATA") (inData = true), sock.write("354 go\r\n");
                else if (cmd === "QUIT") sock.write("221 bye\r\n"), sock.end();
                else sock.write("250 ok\r\n");
            }
        });
    });
    await new Promise<void>((ok) => server.listen(port, "127.0.0.1", ok));

    return {
        mails,
        close: () =>
            new Promise((ok) => {
                for (const s of sockets) s.destroy();
                server.close(() => ok());
            }),
        async waitFor(pred, ms = 15_000) {
            const end = Date.now() + ms;
            while (Date.now() < end) {
                const m = mails.find(pred);
                if (m) return m;
                await new Promise((r) => setTimeout(r, 100));
            }
            throw new Error("no matching mail arrived; got: " + mails.map((m) => m.to.join(",")).join(" | "));
        },
    };
}
