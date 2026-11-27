package sso

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
)

// The "saml" connection type: SAML 2.0 Web Browser SSO, service provider side,
// SP-initiated only. The AuthnRequest goes out over HTTP-Redirect and the
// Response comes back over HTTP-POST to the shared callback route
// (<app url>/api/sso/callback), which is the assertion consumer service.
//
// The XML signature work, the checks of the Response and the Assertion
// (signature by the identity provider's certificate, Issuer, Destination,
// Recipient, Audience, InResponseTo, NotBefore / NotOnOrAfter) and the
// handling of wrapped (XSW) documents are done by github.com/crewjam/saml and
// goxmldsig; nothing here parses a signature. What this file adds:
//
//   - the connection settings (Connection.Config, see docs/inbound-sso.md);
//   - the AuthnRequest ID is derived from the flow's nonce, so the response
//     must answer the request this very flow made (InResponseTo), and a state
//     is used once;
//   - an assertion ID replay cache;
//   - the mapping of NameID and attributes to an Identity.
//
// Unsolicited (IdP-initiated) responses, unsigned responses, encrypted
// assertions and signed AuthnRequests are not supported.

const (
	typeSAML = "saml"

	samlMaxResponse = 1 << 20
	samlSkew        = 60 * time.Second
	samlMetadataTTL = 10 * time.Minute

	samlAttrEmailDefault  = "email"
	samlAttrNameDefault   = "name"
	samlAttrGroupsDefault = "groups"
)

func init() {
	// the library's default allowance is three minutes; assertions live for
	// minutes at the identity providers, so a minute of skew is plenty
	saml.MaxClockSkew = samlSkew

	Register(samlProvider{})

	Presets = append(Presets,
		Preset{
			Id: "okta-saml", Name: "Okta (SAML)", Type: typeSAML,
			IssuerExample: "https://{your-okta-domain}/app/{app-id}/sso/saml/metadata",
			Scopes:        nil, GroupsClaim: "groups",
			Notes: []string{
				"Create a SAML 2.0 app. Single sign-on URL and Audience URI (SP Entity ID): shown by the connection (SP metadata). Name ID format: EmailAddress.",
				"Attribute statements: email = user.email, name = user.displayName, groups = group names (filter by regex).",
				"Paste the app's Identity Provider metadata URL, or its metadata XML.",
			},
		},
		Preset{
			Id: "entra-saml", Name: "Microsoft Entra ID (SAML)", Type: typeSAML,
			IssuerExample: "https://login.microsoftonline.com/{tenant-id}/federationmetadata/2007-06/federationmetadata.xml?appid={app-id}",
			Scopes:        nil, GroupsClaim: "http://schemas.microsoft.com/ws/2008/06/identity/claims/groups",
			Notes: []string{
				"Create an enterprise application (non-gallery) with SAML. Identifier (Entity ID) and Reply URL (ACS): shown by the connection.",
				"Entra sends the claims under their long URIs (emailaddress, displayname, groups); the preset knows them. Groups are object ids.",
				"Use the App Federation Metadata Url of the application, not the tenant-wide one.",
			},
		},
		Preset{
			Id: "google-saml", Name: "Google Workspace (SAML)", Type: typeSAML,
			IssuerExample: "(download the IdP metadata XML from the Admin console and paste it)",
			Scopes:        nil, GroupsClaim: "groups",
			Notes: []string{
				"Admin console > Apps > Web and mobile apps > Add custom SAML app. ACS URL and Entity ID: shown by the connection. Name ID format: EMAIL, name ID: Primary email.",
				"Map attributes: email, name; add a groups attribute with the Google groups to send.",
				"Google offers the IdP metadata only as a download: paste the XML.",
			},
		},
	)
}

type samlProvider struct{}

func (samlProvider) Type() string { return typeSAML }

// samlPreset holds the attribute names a well-known provider sends.
type samlPreset struct{ email, name, groups string }

var samlPresets = map[string]samlPreset{
	"okta": {email: "email", name: "name", groups: "groups"},
	"entra": {
		email:  "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
		name:   "http://schemas.microsoft.com/identity/claims/displayname",
		groups: "http://schemas.microsoft.com/ws/2008/06/identity/claims/groups",
	},
	"google": {email: "email", name: "name", groups: "groups"},
}

// samlSettings is the decoded Connection.Config of a SAML connection.
type samlSettings struct {
	Preset string

	MetadataURL string
	MetadataXML string
	// manual entry, when there is no metadata
	IdPEntityID    string
	IdPSSOURL      string
	IdPCertificate string

	// SPEntityID overrides the entity id (default: the SP metadata URL).
	SPEntityID string

	EmailAttribute  string
	NameAttribute   string
	GroupsAttribute string

	// EmailAuthoritative: the identity provider is authoritative for the
	// connection's verified domains, so the email it asserts counts as
	// verified (it may be linked to an existing account).
	EmailAuthoritative bool
}

func cfgString(m map[string]any, k string) string {
	s, _ := m[k].(string)

	return strings.TrimSpace(s)
}

func samlConfig(c *Connection) samlSettings {
	s := samlSettings{
		Preset:         strings.ToLower(cfgString(c.Config, "preset")),
		MetadataURL:    cfgString(c.Config, "metadataUrl"),
		MetadataXML:    cfgString(c.Config, "metadataXml"),
		IdPEntityID:    cfgString(c.Config, "idpEntityId"),
		IdPSSOURL:      cfgString(c.Config, "idpSsoUrl"),
		IdPCertificate: cfgString(c.Config, "idpCertificate"),
		SPEntityID:     cfgString(c.Config, "spEntityId"),

		EmailAttribute:  cfgString(c.Config, "emailAttribute"),
		NameAttribute:   cfgString(c.Config, "nameAttribute"),
		GroupsAttribute: cfgString(c.Config, "groupsAttribute"),
	}

	s.EmailAuthoritative, _ = c.Config["emailAuthoritative"].(bool)

	p := samlPresets[s.Preset]

	if s.EmailAttribute == "" {
		s.EmailAttribute = firstNonEmpty(p.email, samlAttrEmailDefault)
	}

	if s.NameAttribute == "" {
		s.NameAttribute = firstNonEmpty(p.name, samlAttrNameDefault)
	}

	if s.GroupsAttribute == "" {
		s.GroupsAttribute = firstNonEmpty(p.groups, c.GroupsClaim, samlAttrGroupsDefault)
	}

	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}

	return ""
}

func (samlProvider) Validate(c *Connection) error {
	s := samlConfig(c)

	if s.Preset != "" {
		if _, ok := samlPresets[s.Preset]; !ok {
			return fmt.Errorf("unknown preset %q (okta, entra or google)", s.Preset)
		}
	}

	sources := 0

	if s.MetadataXML != "" {
		sources++

		if _, err := parseIdPMetadata([]byte(s.MetadataXML)); err != nil {
			return fmt.Errorf("the identity provider metadata is not usable: %w", err)
		}
	}

	if s.MetadataURL != "" {
		sources++

		if err := checkEndpointURL(s.MetadataURL); err != nil {
			return fmt.Errorf("metadata URL: %w", err)
		}
	}

	if s.IdPEntityID != "" || s.IdPSSOURL != "" || s.IdPCertificate != "" {
		sources++

		if _, err := manualIdP(s); err != nil {
			return err
		}
	}

	if sources != 1 {
		return errors.New("give exactly one of the identity provider metadata (URL or XML) or the entity id, sign-on URL and certificate")
	}

	if s.SPEntityID != "" && len(s.SPEntityID) > 500 {
		return errors.New("the SP entity id is too long")
	}

	return nil
}

// ---- identity provider description -----------------------------------------------

func parseIdPMetadata(data []byte) (*saml.EntityDescriptor, error) {
	if len(data) > samlMaxResponse {
		return nil, errors.New("metadata too large")
	}

	ed, err := samlsp.ParseMetadata(data)
	if err != nil {
		return nil, err
	}

	return ed, checkIdP(ed)
}

func checkIdP(ed *saml.EntityDescriptor) error {
	if ed.EntityID == "" || len(ed.IDPSSODescriptors) == 0 {
		return errors.New("no identity provider entity in the metadata")
	}

	if ssoLocation(ed) == "" {
		return errors.New("the identity provider offers no HTTP-Redirect single sign-on endpoint")
	}

	return checkEndpointURL(ssoLocation(ed))
}

func ssoLocation(ed *saml.EntityDescriptor) string {
	for _, d := range ed.IDPSSODescriptors {
		for _, s := range d.SingleSignOnServices {
			if s.Binding == saml.HTTPRedirectBinding {
				return strings.TrimSpace(s.Location)
			}
		}
	}

	return ""
}

func manualIdP(s samlSettings) (*saml.EntityDescriptor, error) {
	if s.IdPEntityID == "" || s.IdPSSOURL == "" || s.IdPCertificate == "" {
		return nil, errors.New("the identity provider entity id, sign-on URL and certificate are all required")
	}

	if err := checkEndpointURL(s.IdPSSOURL); err != nil {
		return nil, fmt.Errorf("sign-on URL: %w", err)
	}

	der := []byte(s.IdPCertificate)
	if blk, _ := pem.Decode(der); blk != nil {
		der = blk.Bytes
	} else if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s.IdPCertificate), "")); err == nil {
		der = raw
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errors.New("the certificate is not a valid PEM or base64 X.509 certificate")
	}

	return &saml.EntityDescriptor{
		EntityID: s.IdPEntityID,
		IDPSSODescriptors: []saml.IDPSSODescriptor{{
			SSODescriptor: saml.SSODescriptor{RoleDescriptor: saml.RoleDescriptor{
				ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
				KeyDescriptors: []saml.KeyDescriptor{{
					Use: "signing",
					KeyInfo: saml.KeyInfo{X509Data: saml.X509Data{
						X509Certificates: []saml.X509Certificate{{Data: base64.StdEncoding.EncodeToString(cert.Raw)}},
					}},
				}},
			}},
			SingleSignOnServices: []saml.Endpoint{{Binding: saml.HTTPRedirectBinding, Location: s.IdPSSOURL}},
		}},
	}, nil
}

type metadataEntry struct {
	ed      *saml.EntityDescriptor
	fetched time.Time
}

var (
	metaMu    sync.Mutex
	metaCache = map[string]metadataEntry{}
)

// FetchSAMLMetadata downloads and checks identity provider metadata. The
// request goes through the same client as OIDC discovery: it refuses private
// and loopback addresses unless CLAVIGER_SSO_ALLOW_PRIVATE_NETWORKS is set,
// and https is required.
func FetchSAMLMetadata(ctx context.Context, rawURL string) ([]byte, error) {
	if err := checkEndpointURL(rawURL); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/samlmetadata+xml, application/xml, text/xml")

	res, err := idpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", req.URL.Host, res.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(res.Body, samlMaxResponse+1))
	if err != nil {
		return nil, err
	}

	if len(data) > samlMaxResponse {
		return nil, errors.New("metadata too large")
	}

	return data, nil
}

// idpDescriptor resolves the identity provider of a connection: pasted XML,
// manual entry, or metadata fetched from the URL (cached for ten minutes).
func idpDescriptor(ctx context.Context, s samlSettings) (*saml.EntityDescriptor, error) {
	switch {
	case s.MetadataXML != "":
		return parseIdPMetadata([]byte(s.MetadataXML))
	case s.MetadataURL != "":
		metaMu.Lock()
		e, ok := metaCache[s.MetadataURL]
		metaMu.Unlock()

		if ok && time.Since(e.fetched) < samlMetadataTTL {
			return e.ed, nil
		}

		data, err := FetchSAMLMetadata(ctx, s.MetadataURL)
		if err != nil {
			return nil, fmt.Errorf("metadata fetch failed: %w", err)
		}

		ed, err := parseIdPMetadata(data)
		if err != nil {
			return nil, fmt.Errorf("the identity provider metadata is not usable: %w", err)
		}

		metaMu.Lock()
		metaCache[s.MetadataURL] = metadataEntry{ed: ed, fetched: time.Now()}
		metaMu.Unlock()

		return ed, nil
	default:
		return manualIdP(s)
	}
}

// TestSAML checks that the identity provider description of a connection can
// be loaded (fetching the metadata URL, when that is how it is configured).
func TestSAML(ctx context.Context, c *Connection) error {
	s := samlConfig(c)

	if s.MetadataURL != "" {
		metaMu.Lock()
		delete(metaCache, s.MetadataURL)
		metaMu.Unlock()
	}

	_, err := idpDescriptor(ctx, s)

	return err
}

// ---- service provider ----------------------------------------------------------------

// SAMLMetadataPath is the path of a connection's SP metadata.
func SAMLMetadataPath(connectionId string) string {
	return "/api/sso/" + connectionId + "/saml/metadata"
}

const callbackSuffix = "/api/sso/callback"

func serviceProvider(c *Connection, s samlSettings, callbackURL string, idp *saml.EntityDescriptor) (*saml.ServiceProvider, error) {
	base, ok := strings.CutSuffix(callbackURL, callbackSuffix)
	if !ok {
		return nil, errors.New("unexpected callback URL")
	}

	metaURL, err := url.Parse(base + SAMLMetadataPath(c.Id))
	if err != nil {
		return nil, err
	}

	acs, err := url.Parse(callbackURL)
	if err != nil {
		return nil, err
	}

	sp := &saml.ServiceProvider{
		EntityID:    firstNonEmpty(s.SPEntityID, metaURL.String()),
		MetadataURL: *metaURL,
		AcsURL:      *acs,
		IDPMetadata: idp,
		// the response must answer an AuthnRequest of this flow
		AllowIDPInitiated:                false,
		AuthnNameIDFormat:                saml.EmailAddressNameIDFormat,
		AssertionConsumerServiceBindings: []string{saml.HTTPPostBinding},
		MetadataOmitValidUntil:           true,
		RejectUnknownConditions:          true,
		RejectSHA1Signatures:             true,
		RejectExpiredIDPMetadata:         true,
		ReplayCache:                      replays,
		HTTPClient:                       idpClient(),
	}

	return sp, nil
}

// SAMLServiceProviderMetadata renders the SP metadata a person gives to the
// identity provider: the entity id and the assertion consumer service.
func SAMLServiceProviderMetadata(c *Connection, callbackURL string) ([]byte, error) {
	s := samlConfig(c)

	sp, err := serviceProvider(c, s, callbackURL, nil)
	if err != nil {
		return nil, err
	}

	md := sp.Metadata()

	// plain HTTP-POST ACS, no signing or encryption keys are published
	out, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, err
	}

	return append([]byte(xml.Header), out...), nil
}

func (samlProvider) Begin(ctx context.Context, c *Connection, in BeginInput) (*BeginOutput, error) {
	s := samlConfig(c)

	idp, err := idpDescriptor(ctx, s)
	if err != nil {
		return nil, err
	}

	sp, err := serviceProvider(c, s, in.CallbackURL, idp)
	if err != nil {
		return nil, err
	}

	req, err := sp.MakeAuthenticationRequest(ssoLocation(idp), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return nil, err
	}

	// the request id is derived from the flow's nonce: the response must
	// answer THIS flow (InResponseTo), whichever browser carries it
	req.ID = requestID(in.Nonce)

	u, err := req.Redirect(in.State, sp)
	if err != nil {
		return nil, err
	}

	return &BeginOutput{RedirectURL: u.String()}, nil
}

func requestID(nonce string) string { return "id-" + nonce }

func (samlProvider) Complete(ctx context.Context, c *Connection, in CompleteInput) (*Identity, error) {
	if in.Method != http.MethodPost {
		return nil, newErr(http.StatusBadRequest, "invalid_response", "The identity provider sent no SAML response.")
	}

	raw := in.Form.Get("SAMLResponse")
	if raw == "" || len(raw) > samlMaxResponse*2 {
		return nil, newErr(http.StatusBadRequest, "invalid_response", "The identity provider sent no SAML response.")
	}

	xmlBytes, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, newErr(http.StatusBadRequest, "invalid_response", "The SAML response is not valid.")
	}

	s := samlConfig(c)

	idp, err := idpDescriptor(ctx, s)
	if err != nil {
		return nil, err
	}

	sp, err := serviceProvider(c, s, in.CallbackURL, idp)
	if err != nil {
		return nil, err
	}

	// the ReplayCache is per connection
	sp.ReplayCache = replayScope{c.Id, replays}

	a, err := sp.ParseXMLResponse(xmlBytes, []string{requestID(in.Nonce)}, sp.AcsURL)
	if err != nil {
		return nil, newErr(http.StatusUnauthorized, "invalid_token", "The SAML response could not be verified: "+samlFailure(err))
	}

	return samlIdentity(s, a)
}

// samlFailure names the reason without the response contents.
func samlFailure(err error) string {
	var ive *saml.InvalidResponseError
	if errors.As(err, &ive) && ive.PrivateErr != nil {
		err = ive.PrivateErr
	}

	if err != nil && strings.Contains(err.Error(), "signature element not present") {
		return "unsigned"
	}

	for _, k := range []struct {
		err  error
		name string
	}{
		{saml.ErrAssertionReplayed, "assertion already used"},
		{saml.ErrUntrustedCertificate, "untrusted signing certificate"},
		{saml.ErrInvalidSignature, "signature"},
		{saml.ErrCertificateNotValidNow, "signing certificate not valid"},
		{saml.ErrExpired, "expired"},
		{saml.ErrNotYetValid, "not yet valid"},
		{saml.ErrAudienceMismatch, "audience"},
		{saml.ErrDestinationMismatch, "destination or recipient"},
		{saml.ErrRequestIDMismatch, "in response to another request"},
		{saml.ErrIssuerMismatch, "issuer"},
		{saml.ErrMissingElement, "missing element"},
		{saml.ErrMalformedMessage, "malformed"},
	} {
		if errors.Is(err, k.err) {
			return k.name
		}
	}

	return "not accepted"
}

func samlIdentity(s samlSettings, a *saml.Assertion) (*Identity, error) {
	if a.Subject == nil || a.Subject.NameID == nil || strings.TrimSpace(a.Subject.NameID.Value) == "" {
		return nil, newErr(http.StatusBadGateway, "invalid_response", "The SAML response did not identify the person.")
	}

	nameID := strings.TrimSpace(a.Subject.NameID.Value)

	if a.Subject.NameID.Format == string(saml.TransientNameIDFormat) {
		// a transient id differs at every sign-in: no account can be linked to it
		return nil, newErr(http.StatusBadGateway, "invalid_response", "The identity provider sent a transient name id; configure a persistent or email address name id.")
	}

	attrs := map[string][]string{}

	for _, st := range a.AttributeStatements {
		for _, at := range st.Attributes {
			for _, v := range at.Values {
				if t := strings.TrimSpace(v.Value); t != "" {
					attrs[at.Name] = append(attrs[at.Name], t)
				}
			}
		}
	}

	id := &Identity{
		Subject: nameID,
		Groups:  attrs[s.GroupsAttribute],
		Claims:  map[string]any{"nameId": nameID, "issuer": a.Issuer.Value},
	}

	for k, v := range attrs {
		if len(v) == 1 {
			id.Claims[k] = v[0]
		} else {
			id.Claims[k] = v
		}
	}

	if v := attrs[s.EmailAttribute]; len(v) > 0 {
		id.Email = v[0]
	} else if strings.Contains(nameID, "@") {
		id.Email = nameID
	}

	if v := attrs[s.NameAttribute]; len(v) > 0 {
		id.Name = v[0]
	}

	// SAML has no email_verified. The address is vouched for only when the
	// connection says the identity provider is authoritative for its
	// verified domains; the domain itself is checked by ProvisionLogin.
	id.EmailVerified = s.EmailAuthoritative

	return id, nil
}

// ---- replay cache ------------------------------------------------------------------------

type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

var replays = &replayCache{seen: map[string]time.Time{}}

func (r *replayCache) consume(key string, until time.Time) error {
	sum := sha256.Sum256([]byte(key))
	k := hex.EncodeToString(sum[:])

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for kk, exp := range r.seen {
		if exp.Before(now) {
			delete(r.seen, kk)
		}
	}

	if _, ok := r.seen[k]; ok {
		return saml.ErrAssertionReplayed
	}

	// keep it a little longer than it can be accepted
	r.seen[k] = until.Add(samlSkew)

	return nil
}

// Consume implements saml.AssertionReplayCache for callers without a scope.
func (r *replayCache) Consume(id string, until time.Time) error { return r.consume(id, until) }

type replayScope struct {
	connection string
	cache      *replayCache
}

func (r replayScope) Consume(id string, until time.Time) error {
	return r.cache.consume(r.connection+"\x00"+id, until)
}
