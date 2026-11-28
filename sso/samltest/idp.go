// Package samltest is an in-process SAML 2.0 identity provider for tests: it
// signs responses with a generated certificate, and can produce the broken
// ones the service provider must refuse (unsigned, signed by another key,
// wrong audience, expired, replayed, wrapped).
package samltest

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// IdP is a test identity provider.
type IdP struct {
	EntityID string
	SSOURL   string

	Key  *rsa.PrivateKey
	Cert *x509.Certificate

	// Other signs what the service provider does not trust.
	OtherKey  *rsa.PrivateKey
	OtherCert *x509.Certificate
}

func selfSigned(t testing.TB, cn string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	return key, cert
}

// New makes an identity provider with fresh keys.
func New(t testing.TB) *IdP {
	t.Helper()

	k, c := selfSigned(t, "test idp")
	ok, oc := selfSigned(t, "other idp")

	return &IdP{
		EntityID: "https://idp.test/entity", SSOURL: "https://idp.test/sso",
		Key: k, Cert: c, OtherKey: ok, OtherCert: oc,
	}
}

// MetadataXML is the identity provider's metadata (HTTP-Redirect SSO).
func (i *IdP) MetadataXML() []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
  <md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol" WantAuthnRequestsSigned="false">
    <md:KeyDescriptor use="signing">
      <ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo>
    </md:KeyDescriptor>
    <md:NameIDFormat>urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress</md:NameIDFormat>
    <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s"/>
  </md:IDPSSODescriptor>
</md:EntityDescriptor>`, i.EntityID, base64.StdEncoding.EncodeToString(i.Cert.Raw), i.SSOURL))
}

// CertPEM is the signing certificate as PEM.
func (i *IdP) CertPEM() string {
	return "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(i.Cert.Raw) + "\n-----END CERTIFICATE-----\n"
}

// Request is an AuthnRequest as the identity provider received it.
type Request struct {
	ID          string
	ACS         string
	Issuer      string
	Destination string
	RelayState  string
}

// ParseRedirect reads the AuthnRequest out of the redirect URL the service
// provider sent the browser to.
func (i *IdP) ParseRedirect(location string) (*Request, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(location, i.SSOURL) {
		return nil, fmt.Errorf("not a request to %s: %s", i.SSOURL, location)
	}

	q := u.Query()

	raw, err := base64.StdEncoding.DecodeString(q.Get("SAMLRequest"))
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, err
	}

	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(data); err != nil {
		return nil, err
	}

	root := doc.Root()
	r := &Request{
		ID: root.SelectAttrValue("ID", ""), ACS: root.SelectAttrValue("AssertionConsumerServiceURL", ""),
		Destination: root.SelectAttrValue("Destination", ""), RelayState: q.Get("RelayState"),
	}

	if is := root.SelectElement("Issuer"); is != nil {
		r.Issuer = is.Text()
	}

	if r.ID == "" || r.ACS == "" {
		return nil, fmt.Errorf("incomplete AuthnRequest: %s", data)
	}

	return r, nil
}

// How a response is signed.
const (
	SignAssertion = iota // the usual
	SignResponse
	SignNone
	SignAssertionOtherKey
	SignBoth
)

// How a response is wrapped (XSW).
const (
	WrapNone = iota
	// WrapPrependUnsigned puts an unsigned assertion for another person in
	// front of the signed one.
	WrapPrependUnsigned
	// WrapAppendAfterSign adds an unsigned assertion to a signed response.
	WrapAppendAfterSign
	// WrapCopySignature moves the signature of a genuine assertion onto an
	// assertion for another person.
	WrapCopySignature
)

// Options shape one response. Zero values give a valid response for Email.
type Options struct {
	Email        string
	NameID       string // default: the email
	NameIDFormat string
	Attrs        map[string][]string
	Sign         int
	Wrap         int

	Audience     string
	Recipient    string
	Destination  string
	InResponseTo string
	Issuer       string
	AssertionID  string

	NotBefore    time.Time
	NotOnOrAfter time.Time
	IssueInstant time.Time

	// EvilEmail is whom a wrapped assertion claims to be.
	EvilEmail string
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)

	return "_" + hex.EncodeToString(b)
}

func esc(s string) string {
	var b bytes.Buffer

	_ = xml.EscapeText(&b, []byte(s))

	return b.String()
}

func (i *IdP) assertionXML(req *Request, o Options, email, id string) string {
	now := time.Now()

	nameID := firstNonEmpty(o.NameID, email)
	format := firstNonEmpty(o.NameIDFormat, "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress")

	attrs := ""

	for name, vals := range o.Attrs {
		attrs += fmt.Sprintf(`<saml:Attribute Name="%s">`, esc(name))
		for _, v := range vals {
			attrs += `<saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="xs:string">` + esc(v) + `</saml:AttributeValue>`
		}

		attrs += `</saml:Attribute>`
	}

	return fmt.Sprintf(`<saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="%s" Version="2.0" IssueInstant="%s">`+
		`<saml:Issuer>%s</saml:Issuer>`+
		`<saml:Subject><saml:NameID Format="%s">%s</saml:NameID>`+
		`<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData NotOnOrAfter="%s" Recipient="%s" InResponseTo="%s"/></saml:SubjectConfirmation></saml:Subject>`+
		`<saml:Conditions NotBefore="%s" NotOnOrAfter="%s"><saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction></saml:Conditions>`+
		`<saml:AuthnStatement AuthnInstant="%s"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>`+
		`<saml:AttributeStatement>%s</saml:AttributeStatement>`+
		`</saml:Assertion>`,
		id, ts(firstTime(o.IssueInstant, now)), esc(firstNonEmpty(o.Issuer, i.EntityID)),
		esc(format), esc(nameID),
		ts(firstTime(o.NotOnOrAfter, now.Add(5*time.Minute))), esc(firstNonEmpty(o.Recipient, req.ACS)), esc(firstNonEmpty(o.InResponseTo, req.ID)),
		ts(firstTime(o.NotBefore, now.Add(-time.Minute))), ts(firstTime(o.NotOnOrAfter, now.Add(5*time.Minute))),
		esc(firstNonEmpty(o.Audience, req.Issuer)), ts(now), attrs)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}

	return ""
}

func firstTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}

	return b
}

func parseEl(t testing.TB, s string) *etree.Element {
	t.Helper()

	d := etree.NewDocument()
	if err := d.ReadFromString(s); err != nil {
		t.Fatal(err)
	}

	return d.Root()
}

func (i *IdP) sign(t testing.TB, el *etree.Element, other bool) {
	t.Helper()

	key, cert := i.Key, i.Cert
	if other {
		key, cert = i.OtherKey, i.OtherCert
	}

	ks := dsig.TLSCertKeyStore(tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key})
	ctx := dsig.NewDefaultSigningContext(ks)
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")

	if err := ctx.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
		t.Fatal(err)
	}

	sig, err := ctx.ConstructSignature(el, true)
	if err != nil {
		t.Fatal(err)
	}

	// the signature follows the Issuer
	el.RemoveChild(sig)
	el.InsertChildAt(1, sig)
}

// Response builds a base64 SAMLResponse (the POST form value).
func (i *IdP) Response(t testing.TB, req *Request, o Options) string {
	t.Helper()

	id := firstNonEmpty(o.AssertionID, randID())
	a := parseEl(t, i.assertionXML(req, o, o.Email, id))

	switch o.Sign {
	case SignAssertion, SignBoth:
		i.sign(t, a, false)
	case SignAssertionOtherKey:
		i.sign(t, a, true)
	}

	var extra []*etree.Element

	switch o.Wrap {
	case WrapPrependUnsigned:
		evil := parseEl(t, i.assertionXML(req, o, o.EvilEmail, randID()))
		extra = append(extra, evil)
	case WrapCopySignature:
		evil := parseEl(t, i.assertionXML(req, o, o.EvilEmail, randID()))
		if sig := a.SelectElement("Signature"); sig != nil {
			evil.InsertChildAt(1, sig.Copy())
		}

		extra = append(extra, evil)
	}

	resp := parseEl(t, fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="%s" Version="2.0" IssueInstant="%s" Destination="%s" InResponseTo="%s">`+
		`<saml:Issuer>%s</saml:Issuer><samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status></samlp:Response>`,
		randID(), ts(time.Now()), esc(firstNonEmpty(o.Destination, req.ACS)), esc(firstNonEmpty(o.InResponseTo, req.ID)), esc(firstNonEmpty(o.Issuer, i.EntityID))))

	for _, e := range extra {
		resp.AddChild(e)
	}

	resp.AddChild(a)

	if o.Sign == SignResponse || o.Sign == SignBoth {
		i.sign(t, resp, false)
	}

	if o.Wrap == WrapAppendAfterSign {
		resp.AddChild(parseEl(t, i.assertionXML(req, o, o.EvilEmail, randID())))
	}

	doc := etree.NewDocument()
	doc.SetRoot(resp)

	out, err := doc.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(out)
}
