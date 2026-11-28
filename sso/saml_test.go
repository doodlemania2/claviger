package sso

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/sso/samltest"
)

const testCallback = "https://app.test/api/sso/callback"

func samlConn(idp *samltest.IdP, extra map[string]any) *Connection {
	cfg := map[string]any{"metadataXml": string(idp.MetadataXML())}
	for k, v := range extra {
		cfg[k] = v
	}

	return &Connection{Id: "conn1", Type: "saml", Config: cfg, GroupsClaim: "groups"}
}

// begin starts a sign-in and returns what the identity provider received.
func begin(t *testing.T, idp *samltest.IdP, c *Connection, nonce string) *samltest.Request {
	t.Helper()

	out, err := samlProvider{}.Begin(context.Background(), c, BeginInput{CallbackURL: testCallback, State: "state-" + nonce, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}

	req, err := idp.ParseRedirect(out.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}

	if req.RelayState != "state-"+nonce || req.ACS != testCallback || req.ID != "id-"+nonce {
		t.Fatalf("unexpected AuthnRequest: %+v", req)
	}

	if want := "https://app.test/api/sso/conn1/saml/metadata"; req.Issuer != want {
		t.Fatalf("issuer = %q, want %q", req.Issuer, want)
	}

	return req
}

func complete(c *Connection, nonce, response string) (*Identity, error) {
	return samlProvider{}.Complete(context.Background(), c, CompleteInput{
		CallbackURL: testCallback, Nonce: nonce, Method: http.MethodPost,
		Form: url.Values{"SAMLResponse": {response}, "RelayState": {"state-" + nonce}},
	})
}

func TestSAMLResponseVerification(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name    string
		opts    samltest.Options
		nonce2  string // when set, the response answers a different flow
		wantErr string // "" = accepted
	}{
		{name: "assertion signed", opts: samltest.Options{Email: "a@example.org"}},
		{name: "response signed", opts: samltest.Options{Email: "a@example.org", Sign: samltest.SignResponse}},
		{name: "both signed", opts: samltest.Options{Email: "a@example.org", Sign: samltest.SignBoth}},
		{name: "unsigned", opts: samltest.Options{Email: "a@example.org", Sign: samltest.SignNone}, wantErr: "unsigned"},
		{name: "signed by another key", opts: samltest.Options{Email: "a@example.org", Sign: samltest.SignAssertionOtherKey}, wantErr: "untrusted"},
		{name: "wrong audience", opts: samltest.Options{Email: "a@example.org", Audience: "https://other.test/sp"}, wantErr: "audience"},
		{name: "wrong recipient", opts: samltest.Options{Email: "a@example.org", Recipient: "https://other.test/acs"}, wantErr: "destination"},
		{name: "wrong destination", opts: samltest.Options{Email: "a@example.org", Destination: "https://other.test/acs", Sign: samltest.SignBoth}, wantErr: "destination"},
		{name: "expired", opts: samltest.Options{Email: "a@example.org", NotBefore: now.Add(-time.Hour), NotOnOrAfter: now.Add(-30 * time.Minute), IssueInstant: now.Add(-time.Hour)}, wantErr: "expired"},
		{name: "not yet valid", opts: samltest.Options{Email: "a@example.org", NotBefore: now.Add(time.Hour), NotOnOrAfter: now.Add(2 * time.Hour)}, wantErr: "not yet valid"},
		{name: "wrong issuer", opts: samltest.Options{Email: "a@example.org", Issuer: "https://evil.test/idp"}, wantErr: "issuer"},
		{name: "InResponseTo another request", opts: samltest.Options{Email: "a@example.org", InResponseTo: "id-someoneelse"}, wantErr: "in response"},
		{name: "answers another flow", opts: samltest.Options{Email: "a@example.org"}, nonce2: "othernonce00000000000000000000", wantErr: "in response"},
		{name: "XSW unsigned assertion prepended", opts: samltest.Options{Email: "a@example.org", Wrap: samltest.WrapPrependUnsigned, EvilEmail: "ceo@example.org"}, wantErr: "x"},
		{name: "XSW unsigned assertion added to a signed response", opts: samltest.Options{Email: "a@example.org", Sign: samltest.SignResponse, Wrap: samltest.WrapAppendAfterSign, EvilEmail: "ceo@example.org"}, wantErr: "x"},
		{name: "XSW signature copied onto another assertion", opts: samltest.Options{Email: "a@example.org", Wrap: samltest.WrapCopySignature, EvilEmail: "ceo@example.org"}, wantErr: "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp := samltest.New(t)
			c := samlConn(idp, nil)
			nonce := "nonce1234567890123456789012345678"
			req := begin(t, idp, c, nonce)

			answer := nonce
			if tc.nonce2 != "" {
				answer = tc.nonce2
			}

			id, err := complete(c, answer, idp.Response(t, req, tc.opts))

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the response to be accepted: %v", err)
				}

				if id.Email != "a@example.org" || id.Subject != "a@example.org" {
					t.Fatalf("unexpected identity: %+v", id)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected a refusal, got identity %+v", id)
			}

			e, ok := AsError(err)
			if !ok || e.Code != "invalid_token" {
				t.Fatalf("expected an invalid_token error, got %v", err)
			}

			if tc.wantErr == "x" {
				t.Logf("reason: %s", e.Message)
			}

			if tc.wantErr != "x" && !strings.Contains(e.Message, tc.wantErr) {
				t.Fatalf("expected the reason to mention %q: %v", tc.wantErr, err)
			}

			if id != nil {
				t.Fatalf("no identity may come out of a refused response: %+v", id)
			}
		})
	}
}

func TestSAMLAssertionReplayed(t *testing.T) {
	idp := samltest.New(t)
	c := samlConn(idp, nil)
	nonce := "nonce1234567890123456789012345678"
	req := begin(t, idp, c, nonce)

	resp := idp.Response(t, req, samltest.Options{Email: "a@example.org", AssertionID: "_fixedassertionid"})

	if _, err := complete(c, nonce, resp); err != nil {
		t.Fatal(err)
	}

	// the same response again, even with the right InResponseTo
	_, err := complete(c, nonce, resp)
	if e, ok := AsError(err); !ok || !strings.Contains(e.Message, "already used") {
		t.Fatalf("a replayed assertion must be refused: %v", err)
	}

	// the same assertion id on a second flow of the same connection is a replay too
	nonce2 := "nonce2222222222222222222222222222"
	req2 := begin(t, idp, c, nonce2)

	_, err = complete(c, nonce2, idp.Response(t, req2, samltest.Options{Email: "a@example.org", AssertionID: "_fixedassertionid"}))
	if e, ok := AsError(err); !ok || !strings.Contains(e.Message, "already used") {
		t.Fatalf("a reused assertion id must be refused: %v", err)
	}
}

func TestSAMLRequiresPOST(t *testing.T) {
	idp := samltest.New(t)
	c := samlConn(idp, nil)

	_, err := samlProvider{}.Complete(context.Background(), c, CompleteInput{CallbackURL: testCallback, Method: http.MethodGet, Query: url.Values{"SAMLResponse": {"x"}}})
	if err == nil {
		t.Fatal("a SAML response must arrive by POST")
	}
}

func TestSAMLAttributeMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  map[string]any
		opts   samltest.Options
		email  string
		nameV  string
		groups []string
		verify bool
	}{
		{
			name: "defaults, email from the attribute", opts: samltest.Options{
				Email: "x@example.org", NameID: "persistent-1", NameIDFormat: "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent",
				Attrs: map[string][]string{"email": {"real@example.org"}, "name": {"Real Person"}, "groups": {"eng", "ops"}},
			},
			email: "real@example.org", nameV: "Real Person", groups: []string{"eng", "ops"},
		},
		{
			name: "email falls back to the NameID", opts: samltest.Options{Email: "n@example.org"},
			email: "n@example.org",
		},
		{
			name: "custom attribute names, authoritative", extra: map[string]any{
				"emailAttribute": "mail", "nameAttribute": "cn", "groupsAttribute": "memberOf", "emailAuthoritative": true,
			},
			opts: samltest.Options{Email: "n@example.org", Attrs: map[string][]string{
				"mail": {"m@example.org"}, "cn": {"Cee Enn"}, "memberOf": {"g1"},
			}},
			email: "m@example.org", nameV: "Cee Enn", groups: []string{"g1"}, verify: true,
		},
		{
			name: "entra preset", extra: map[string]any{"preset": "entra"},
			opts: samltest.Options{Email: "n@example.org", Attrs: map[string][]string{
				"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {"e@example.org"},
				"http://schemas.microsoft.com/identity/claims/displayname":           {"Entra Person"},
				"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups":     {"guid-1", "guid-2"},
			}},
			email: "e@example.org", nameV: "Entra Person", groups: []string{"guid-1", "guid-2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp := samltest.New(t)
			c := samlConn(idp, tc.extra)
			nonce := "nonce1234567890123456789012345678"
			req := begin(t, idp, c, nonce)

			id, err := complete(c, nonce, idp.Response(t, req, tc.opts))
			if err != nil {
				t.Fatal(err)
			}

			if id.Email != tc.email || id.Name != tc.nameV || id.EmailVerified != tc.verify || strings.Join(id.Groups, ",") != strings.Join(tc.groups, ",") {
				t.Fatalf("unexpected identity: %+v", id)
			}
		})
	}
}

func TestSAMLTransientNameIDRefused(t *testing.T) {
	idp := samltest.New(t)
	c := samlConn(idp, nil)
	nonce := "nonce1234567890123456789012345678"
	req := begin(t, idp, c, nonce)

	_, err := complete(c, nonce, idp.Response(t, req, samltest.Options{
		Email: "a@example.org", NameIDFormat: "urn:oasis:names:tc:SAML:2.0:nameid-format:transient",
	}))
	if err == nil {
		t.Fatal("a transient name id cannot identify a person across sign-ins")
	}
}

func TestSAMLValidate(t *testing.T) {
	idp := samltest.New(t)

	for _, tc := range []struct {
		name string
		cfg  map[string]any
		ok   bool
	}{
		{"metadata xml", map[string]any{"metadataXml": string(idp.MetadataXML())}, true},
		{"metadata url", map[string]any{"metadataUrl": "https://idp.example.org/metadata"}, true},
		{"manual", map[string]any{"idpEntityId": "e", "idpSsoUrl": "https://idp.example.org/sso", "idpCertificate": idp.CertPEM()}, true},
		{"nothing", map[string]any{}, false},
		{"two sources", map[string]any{"metadataXml": string(idp.MetadataXML()), "metadataUrl": "https://idp.example.org/m"}, false},
		{"garbage xml", map[string]any{"metadataXml": "<nope"}, false},
		{"http url", map[string]any{"metadataUrl": "http://idp.example.org/m"}, false},
		{"manual without a certificate", map[string]any{"idpEntityId": "e", "idpSsoUrl": "https://idp.example.org/sso"}, false},
		{"manual with a bad certificate", map[string]any{"idpEntityId": "e", "idpSsoUrl": "https://idp.example.org/sso", "idpCertificate": "nope"}, false},
		{"unknown preset", map[string]any{"metadataXml": string(idp.MetadataXML()), "preset": "nope"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := samlProvider{}.Validate(&Connection{Id: "c", Type: "saml", Config: tc.cfg})
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestSAMLMetadataFromURLAndManual(t *testing.T) {
	idp := samltest.New(t)

	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++

		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(idp.MetadataXML())
	}))
	defer srv.Close()

	t.Run("fetched, then cached", func(t *testing.T) {
		defer SetHTTPClient(srv.Client())()

		c := &Connection{Id: "conn1", Type: "saml", Config: map[string]any{"metadataUrl": srv.URL + "/m"}}

		nonce := "nonce1234567890123456789012345678"
		req := begin(t, idp, c, nonce)

		if _, err := complete(c, nonce, idp.Response(t, req, samltest.Options{Email: "a@example.org"})); err != nil {
			t.Fatal(err)
		}

		if hits != 1 {
			t.Fatalf("the metadata should be fetched once and cached, got %d fetches", hits)
		}

		if err := TestSAML(context.Background(), c); err != nil || hits != 2 {
			t.Fatalf("the test action refetches: err=%v hits=%d", err, hits)
		}
	})

	t.Run("the private network guard applies", func(t *testing.T) {
		// the default client refuses the loopback address the test server listens on
		c := &Connection{Id: "conn1", Type: "saml", Config: map[string]any{"metadataUrl": srv.URL + "/other"}}

		if err := TestSAML(context.Background(), c); err == nil {
			t.Fatal("metadata on a loopback address must not be fetched")
		}
	})

	t.Run("a fetch that fails is an error, not an empty provider", func(t *testing.T) {
		bad := httptest.NewTLSServer(http.NotFoundHandler())
		defer bad.Close()

		defer SetHTTPClient(bad.Client())()

		c := &Connection{Id: "conn1", Type: "saml", Config: map[string]any{"metadataUrl": bad.URL + "/m"}}
		if err := TestSAML(context.Background(), c); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("manual entry", func(t *testing.T) {
		c := &Connection{Id: "conn1", Type: "saml", Config: map[string]any{
			"idpEntityId": idp.EntityID, "idpSsoUrl": idp.SSOURL, "idpCertificate": idp.CertPEM(),
		}}

		nonce := "nonce1234567890123456789012345678"
		req := begin(t, idp, c, nonce)

		if _, err := complete(c, nonce, idp.Response(t, req, samltest.Options{Email: "a@example.org"})); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSAMLServiceProviderMetadata(t *testing.T) {
	idp := samltest.New(t)
	c := samlConn(idp, nil)

	xmlBytes, err := SAMLServiceProviderMetadata(c, testCallback)
	if err != nil {
		t.Fatal(err)
	}

	s := string(xmlBytes)

	for _, want := range []string{
		`entityID="https://app.test/api/sso/conn1/saml/metadata"`,
		`Location="https://app.test/api/sso/callback"`,
		`urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SP metadata lacks %s:\n%s", want, s)
		}
	}

	c.Config["spEntityId"] = "urn:custom:sp"

	xmlBytes, _ = SAMLServiceProviderMetadata(c, testCallback)
	if !strings.Contains(string(xmlBytes), `entityID="urn:custom:sp"`) {
		t.Errorf("the configured entity id is not used:\n%s", xmlBytes)
	}
}
