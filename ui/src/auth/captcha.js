// Fork-local (Claviger #91): optional CAPTCHA widget for the admin login.
//
// The provider script is loaded ONLY when auth-methods reports that the action
// requires a captcha, so a default install never contacts a third party. The
// admin UI CSP is widened for the chosen provider by the server in that case.

const SRC = {
    turnstile: () => "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit",
    hcaptcha: () => "https://js.hcaptcha.com/1/api.js?render=explicit",
    recaptchaV2: () => "https://www.google.com/recaptcha/api.js?render=explicit",
    recaptchaV3: (cfg) => "https://www.google.com/recaptcha/api.js?render=" + encodeURIComponent(cfg.siteKey),
};

const loaded = {};

function loadScript(cfg) {
    const src = SRC[cfg.provider]?.(cfg);
    if (!src) {
        return Promise.reject(new Error("Unsupported captcha provider."));
    }

    if (!loaded[src]) {
        loaded[src] = new Promise((resolve, reject) => {
            const el = document.createElement("script");
            el.src = src;
            el.async = true;
            el.onload = resolve;
            el.onerror = () => {
                delete loaded[src];
                reject(new Error("Could not load the captcha."));
            };
            document.head.appendChild(el);
        });
    }

    return loaded[src];
}

/**
 * Returns true when the auth-methods response requires a captcha for the action.
 */
export function captchaRequired(authMethods, action) {
    return !!authMethods?.captcha?.required?.includes(action);
}

/**
 * Resolves with a captcha token for the action (rendering the widget inside
 * the container element when the provider needs a visible challenge), or
 * with an empty string when the action does not need one.
 */
export async function getCaptchaToken(authMethods, action, container) {
    if (!captchaRequired(authMethods, action)) {
        return "";
    }

    const cfg = authMethods.captcha;

    await loadScript(cfg);

    if (cfg.provider === "recaptchaV3") {
        await new Promise((resolve) => window.grecaptcha.ready(resolve));
        return window.grecaptcha.execute(cfg.siteKey, { action });
    }

    if (cfg.provider === "recaptchaV2") {
        await new Promise((resolve) => window.grecaptcha.ready(resolve));
    }

    const api = cfg.provider === "turnstile"
        ? window.turnstile
        : cfg.provider === "hcaptcha"
        ? window.hcaptcha
        : window.grecaptcha;

    if (!container) {
        throw new Error("Missing captcha container.");
    }

    container.hidden = false;
    container.textContent = "";
    const holder = document.createElement("div");
    container.appendChild(holder);

    return new Promise((resolve, reject) => {
        const finish = (fn, value) => {
            container.hidden = true;
            container.textContent = "";
            fn(value);
        };

        api.render(holder, {
            sitekey: cfg.siteKey,
            action: cfg.provider === "turnstile" ? action : undefined,
            callback: (token) => finish(resolve, token),
            "error-callback": () => finish(reject, new Error("Captcha failed, please try again.")),
            "expired-callback": () => finish(reject, new Error("Captcha expired, please try again.")),
        });
    });
}

/**
 * Request options (headers) carrying the token for the pb SDK calls.
 */
export function captchaOptions(token) {
    return token ? { headers: { "X-Captcha-Token": token } } : {};
}
