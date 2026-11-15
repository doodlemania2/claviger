/**
 * The on-screen helpers every actor gets: a caption bar, a name badge, a click ripple, a cursor dot and
 * a "card" for things that have no screen (an API call and its answer, a command and its output).
 * They are built with DOM calls and CSSOM (`el.style`), never with an injected <style> or <script>,
 * because Claviger's Content-Security-Policy refuses those. The init script runs again on each full
 * page load and puts the last caption back from sessionStorage.
 *
 * It only acts on top-level pages of localhost, never inside a frame (the sandboxed certificate
 * frame, for one), so a recording never shows it on another site.
 */
// This function is sent to the browser whole (addInitScript), so it cannot call helpers outside itself.
export function overlayInit(): void {
    if (window.top !== window || location.hostname !== "localhost") return;
    const KEY = "walk-caption";
    const LABEL = "walk-label";
    const root = (): HTMLElement => document.documentElement;
    const css = (el: HTMLElement, props: Record<string, string>): void => {
        for (const [k, v] of Object.entries(props)) el.style.setProperty(k, v);
    };
    const make = (id: string, props: Record<string, string>): HTMLElement => {
        let el = document.getElementById(id);
        if (!el) {
            el = document.createElement("div");
            el.id = id;
            el.setAttribute("aria-hidden", "true");
            root().appendChild(el);
        }
        css(el, { position: "fixed", "z-index": "2147483647", "pointer-events": "none", ...props });
        return el;
    };
    let timer = 0;
    const caption = (text: string | null): void => {
        try {
            if (text) sessionStorage.setItem(KEY, text);
            else sessionStorage.removeItem(KEY);
        } catch {
            /* storage can be blocked */
        }
        const el = make("walk-caption", {
            left: "50%",
            bottom: "18px",
            transform: "translateX(-50%)",
            "max-width": "92vw",
            padding: "10px 18px",
            background: "rgba(15, 23, 42, 0.92)",
            color: "#fff",
            font: "600 18px/1.35 system-ui, sans-serif",
            "border-radius": "12px",
            "text-align": "center",
            "box-shadow": "0 4px 18px rgba(0,0,0,.35)",
            display: text ? "block" : "none",
        });
        el.textContent = text ?? "";
        clearTimeout(timer);
        // A caption stays long enough to read, then goes, so it never describes a screen that moved on.
        if (text) timer = window.setTimeout(() => caption(null), 9000);
    };
    const label = (text: string | null): void => {
        try {
            if (text) sessionStorage.setItem(LABEL, text);
        } catch {
            /* storage can be blocked */
        }
        const el = make("walk-label", {
            left: "10px",
            top: "10px",
            padding: "3px 10px",
            background: "rgba(30, 64, 175, 0.92)",
            color: "#fff",
            font: "700 13px/1.3 system-ui, sans-serif",
            "border-radius": "999px",
            display: text ? "block" : "none",
        });
        el.textContent = text ?? "";
    };
    // A card shows what has no screen: a title and some monospace lines, until it is cleared.
    const card = (title: string | null, lines?: string[]): void => {
        const box = make("walk-card", {
            left: "50%",
            top: "50%",
            transform: "translate(-50%, -50%)",
            width: "min(92vw, 940px)",
            "max-height": "80vh",
            overflow: "hidden",
            padding: "18px 22px",
            background: "rgba(15, 23, 42, 0.97)",
            color: "#e2e8f0",
            "border-radius": "14px",
            "box-shadow": "0 10px 40px rgba(0,0,0,.55)",
            border: "1px solid #334155",
            display: title ? "block" : "none",
        });
        box.textContent = "";
        if (!title) return;
        const head = document.createElement("div");
        head.textContent = title;
        css(head, { font: "700 18px/1.3 system-ui, sans-serif", color: "#fbbf24", "margin-bottom": "10px" });
        box.appendChild(head);
        for (const line of lines ?? []) {
            const row = document.createElement("div");
            row.textContent = line;
            css(row, {
                font: "500 14px/1.45 ui-monospace, Menlo, monospace",
                "white-space": "pre-wrap",
                "word-break": "break-all",
                color: line.startsWith("<-") ? "#86efac" : line.startsWith("->") ? "#93c5fd" : "#e2e8f0",
            });
            box.appendChild(row);
        }
    };
    const ripple = (x: number, y: number): void => {
        const el = make(`walk-ripple-${String(Date.now())}`, {
            left: `${String(x - 18)}px`,
            top: `${String(y - 18)}px`,
            width: "36px",
            height: "36px",
            "border-radius": "50%",
            border: "3px solid #f59e0b",
            background: "rgba(245, 158, 11, 0.25)",
        });
        const anim = el.animate(
            [
                { transform: "scale(0.4)", opacity: 1 },
                { transform: "scale(1.6)", opacity: 0 },
            ],
            { duration: 550, easing: "ease-out" },
        );
        anim.onfinish = () => {
            el.remove();
        };
    };
    const cursor = (x: number, y: number): void => {
        make("walk-cursor", {
            left: `${String(x - 6)}px`,
            top: `${String(y - 6)}px`,
            width: "12px",
            height: "12px",
            "border-radius": "50%",
            background: "#ef4444",
            border: "2px solid #fff",
            "box-shadow": "0 0 4px rgba(0,0,0,.6)",
        });
    };
    Object.assign(window, { __walkCaption: caption, __walkLabel: label, __walkCard: card });
    addEventListener(
        "pointerdown",
        (e) => {
            ripple(e.clientX, e.clientY);
        },
        true,
    );
    addEventListener(
        "pointermove",
        (e) => {
            if (e.pointerType === "mouse") cursor(e.clientX, e.clientY);
        },
        true,
    );
    const restore = (): void => {
        try {
            const c = sessionStorage.getItem(KEY);
            const l = sessionStorage.getItem(LABEL);
            if (c) caption(c);
            if (l) label(l);
        } catch {
            /* storage can be blocked */
        }
    };
    if (document.readyState === "loading") addEventListener("DOMContentLoaded", restore);
    else restore();
}
