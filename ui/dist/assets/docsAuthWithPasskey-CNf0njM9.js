function e(e){let a=app.utils.getApiExampleURL(),o=[{title:`Register passkey`,content:n},{title:`Login with passkey`,content:r},{title:`Manage credentials`,content:i}],s=store({activeActionIndex:0});return t.div({pbEvent:`apiPreviewAuthWithPasskey`,className:`content`},t.p(null,`Register and authenticate ${e.name} records using WebAuthn-based passkeys (platform authenticators, hardware keys, etc.).`),t.p(null,`Both the registration and login flows are two-step: the client requests a challenge `,`(`,t.code(null,`*-begin`),`), runs the browser WebAuthn ceremony, then sends the result back `,`(`,t.code(null,`*-finish`),`).`),app.components.codeBlockTabs({className:`sdk-examples m-t-sm`,historyKey:`pbLastSDK`,tabs:[{title:`Browser (fetch + WebAuthn)`,language:`js`,value:`
                        // Minimal browser flow (no SDK helpers required).
                        //
                        // Helpers (base64url <-> ArrayBuffer):
                        const b64uToBuf = (s) => {
                            s = String(s).replace(/-/g, "+").replace(/_/g, "/");
                            while (s.length % 4) s += "=";
                            const bin = atob(s);
                            const buf = new Uint8Array(bin.length);
                            for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
                            return buf.buffer;
                        };
                        const bufToB64u = (buf) => {
                            const bytes = new Uint8Array(buf);
                            let bin = "";
                            for (let i = 0; i < bytes.byteLength; i++) bin += String.fromCharCode(bytes[i]);
                            return btoa(bin).replace(/\\+/g, "-").replace(/\\//g, "_").replace(/=+$/, "");
                        };

                        // ---
                        // Register a new passkey for the currently signed-in record
                        // ---
                        const begin = await fetch('${a}/api/collections/${e.name}/auth-with-webauthn/register-begin', {
                            method: 'POST',
                            headers: { Authorization: AUTH_TOKEN },
                        }).then((r) => r.json());

                        let publicKey = begin.options?.publicKey || begin.publicKey || begin.options || begin;
                        publicKey.challenge = b64uToBuf(publicKey.challenge);
                        publicKey.user.id  = b64uToBuf(publicKey.user.id);

                        const cred = await navigator.credentials.create({ publicKey });

                        await fetch('${a}/api/collections/${e.name}/auth-with-webauthn/register-finish', {
                            method: 'POST',
                            headers: { 'Content-Type': 'application/json', Authorization: AUTH_TOKEN },
                            body: JSON.stringify({
                                sessionToken: begin.sessionToken,
                                name: 'My iPhone',
                                id: cred.id,
                                rawId: bufToB64u(cred.rawId),
                                type: cred.type,
                                response: {
                                    attestationObject: bufToB64u(cred.response.attestationObject),
                                    clientDataJSON: bufToB64u(cred.response.clientDataJSON),
                                },
                            }),
                        });

                        // ---
                        // Login with a passkey (unauthenticated)
                        // ---
                        const lb = await fetch('${a}/api/collections/${e.name}/auth-with-webauthn/login-begin', {
                            method: 'POST',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ identity: 'test@example.com' }),
                        }).then((r) => r.json());

                        let lpk = lb.options?.publicKey || lb.publicKey || lb.options || lb;
                        lpk.challenge = b64uToBuf(lpk.challenge);
                        if (Array.isArray(lpk.allowCredentials)) {
                            lpk.allowCredentials = lpk.allowCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
                        }

                        const assertion = await navigator.credentials.get({ publicKey: lpk });

                        const auth = await fetch('${a}/api/collections/${e.name}/auth-with-webauthn/login-finish', {
                            method: 'POST',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({
                                identity: 'test@example.com',
                                sessionToken: lb.sessionToken,
                                id: assertion.id,
                                rawId: bufToB64u(assertion.rawId),
                                type: assertion.type,
                                response: {
                                    authenticatorData: bufToB64u(assertion.response.authenticatorData),
                                    clientDataJSON: bufToB64u(assertion.response.clientDataJSON),
                                    signature: bufToB64u(assertion.response.signature),
                                    userHandle: assertion.response.userHandle ? bufToB64u(assertion.response.userHandle) : null,
                                },
                            }),
                        }).then((r) => r.json());

                        // auth = { token, record }
                    `},{title:`curl`,language:`bash`,value:`
                        # ---
                        # Register: step 1 (authenticated as the record)
                        # ---
                        curl -X POST \\
                          -H "Authorization: AUTH_TOKEN" \\
                          '${a}/api/collections/${e.name}/auth-with-webauthn/register-begin'

                        # ---
                        # Register: step 2 (authenticated as the record)
                        # ---
                        curl -X POST \\
                          -H 'Content-Type:application/json' \\
                          -H "Authorization: AUTH_TOKEN" \\
                          -d '{ "sessionToken":"...", "name":"...", "id":"...", "rawId":"...", "type":"public-key", "response": { "attestationObject":"...", "clientDataJSON":"..." } }' \\
                          '${a}/api/collections/${e.name}/auth-with-webauthn/register-finish'

                        # ---
                        # Login: step 1 (unauthenticated)
                        # ---
                        curl -X POST \\
                          -H 'Content-Type:application/json' \\
                          -d '{ "identity":"test@example.com" }' \\
                          '${a}/api/collections/${e.name}/auth-with-webauthn/login-begin'

                        # ---
                        # Login: step 2 (unauthenticated)
                        # ---
                        curl -X POST \\
                          -H 'Content-Type:application/json' \\
                          -d '{ "identity":"test@example.com", "sessionToken":"...", "id":"...", "rawId":"...", "type":"public-key", "response": { "authenticatorData":"...", "clientDataJSON":"...", "signature":"...", "userHandle":null } }' \\
                          '${a}/api/collections/${e.name}/auth-with-webauthn/login-finish'
                    `}]}),t.nav({className:`btns m-t-base m-b-sm`},()=>o.map((e,n)=>t.button({type:`button`,className:()=>`btn sm expanded ${s.activeActionIndex==n?`active`:`secondary`}`,textContent:()=>e.title,onclick:()=>s.activeActionIndex=n}))),()=>o[s.activeActionIndex]?.content?.(e))}function n(e){return[t.div({className:`block`},t.strong(null,`API details`)),t.div({className:`alert success api-preview-alert`},t.span({className:`label method`},`POST`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/register-begin`)),t.p({className:`txt-hint`},`Requires the auth `,t.code(null,`Authorization`),` header of the record that will own the new passkey. Returns `,t.code(null,`{ options, sessionToken }`),` — pass `,t.code(null,`options.publicKey`),` to `,t.code(null,`navigator.credentials.create()`),`.`),t.div({className:`alert success api-preview-alert m-t-sm`},t.span({className:`label method`},`POST`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/register-finish`)),t.table({className:`api-preview-table body-params`},t.thead(null,t.tr(null,t.th({className:`min-width txt-primary`},`Body params`),t.th({className:`min-width`},`Type`),t.th(null,`Description`))),t.tbody(null,t.tr(null,t.td({className:`min-width`},`sessionToken `,t.em(null,`(required)`)),t.td({className:`min-width`},t.span({className:`label`},`String`)),t.td(null,`The session token returned by `,t.code(null,`register-begin`),`.`)),t.tr(null,t.td({className:`min-width`},`name`),t.td({className:`min-width`},t.span({className:`label`},`String`)),t.td(null,`Optional human-readable label for the credential (e.g. "My iPhone").`)),t.tr(null,t.td({className:`min-width`},`id, rawId, type, response.*`),t.td({className:`min-width`},t.span({className:`label`},`WebAuthn`)),t.td(null,`Attestation produced by `,t.code(null,`navigator.credentials.create()`),` — base64url-encode all `,t.code(null,`ArrayBuffer`),` fields.`))))]}function r(e){return[t.div({className:`block`},t.strong(null,`API details`)),t.div({className:`alert success api-preview-alert`},t.span({className:`label method`},`POST`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/login-begin`)),t.table({className:`api-preview-table body-params`},t.thead(null,t.tr(null,t.th({className:`min-width txt-primary`},`Body params`),t.th({className:`min-width`},`Type`),t.th(null,`Description`))),t.tbody(null,t.tr(null,t.td({className:`min-width`},`identity `,t.em(null,`(required)`)),t.td({className:`min-width`},t.span({className:`label`},`String`)),t.td(null,`Email or username of the record to sign in.`)))),t.p({className:`txt-hint m-t-xs`},`Returns `,t.code(null,`{ options, sessionToken }`),`. Pass `,t.code(null,`options.publicKey`),` to `,t.code(null,`navigator.credentials.get()`),`.`),t.div({className:`alert success api-preview-alert m-t-sm`},t.span({className:`label method`},`POST`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/login-finish`)),t.p({className:`txt-hint`},`Body: `,t.code(null,`{ identity, sessionToken, id, rawId, type, response: { authenticatorData, clientDataJSON, signature, userHandle } }`),`. On success returns the standard auth response `,t.code(null,`{ token, record }`),`.`)]}function i(e){return[t.div({className:`block`},t.strong(null,`API details`)),t.div({className:`alert success api-preview-alert`},t.span({className:`label method`},`GET`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/credentials`)),t.p({className:`txt-hint`},`Lists the authenticated record's registered passkeys.`),t.div({className:`alert success api-preview-alert m-t-sm`},t.span({className:`label method`},`PATCH`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/credentials/{credentialId}`)),t.p({className:`txt-hint`},`Renames a passkey. Body: `,t.code(null,`{ "name": "..." }`),`.`),t.div({className:`alert success api-preview-alert m-t-sm`},t.span({className:`label method`},`DELETE`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/credentials/{credentialId}`)),t.p({className:`txt-hint`},`Removes a passkey from the authenticated record.`),t.div({className:`alert success api-preview-alert m-t-sm`},t.span({className:`label method`},`DELETE`),t.span({className:`path`},`/api/collections/${e.name}/auth-with-webauthn/credentials-by-record/{id}`)),t.p({className:`txt-hint`},`Superuser-only. Clears `,t.strong(null,`all`),` passkeys for the specified record (account recovery).`)]}export{e as docsAuthWithPasskey};