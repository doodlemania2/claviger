package qr_test

import (
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tools/qr"
)

func TestQRSVGStructure(t *testing.T) {
	t.Parallel()

	const url = "https://auth.example.org/recovery#code=ABCDEFGHJKLMNPQRSTUVWXYZ23"

	out, err := qr.SVG(url, `Recovery <link> "x"`)
	if err != nil {
		t.Fatal(err)
	}

	// well-formed XML
	var doc struct {
		XMLName xml.Name `xml:"svg"`
		Title   string   `xml:"title"`
	}
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, out)
	}
	if doc.Title != `Recovery <link> "x"` {
		t.Fatalf("title %q", doc.Title)
	}

	for _, want := range []string{`role="img"`, `aria-label="Recovery &lt;link&gt; &quot;x&quot;"`, `viewBox="0 0 `} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}

	// nothing a strict CSP would block or a reader could abuse, and the
	// encoded text is not in the markup
	for _, bad := range []string{"<script", "style=", "href", "http://", "https://auth", "ABCDEFGH", "onload"} {
		if strings.Contains(strings.Replace(out, "http://www.w3.org/2000/svg", "", 1), bad) {
			t.Fatalf("unexpected %q in output", bad)
		}
	}

	// a version-N symbol is 17+4N modules plus the 8 module quiet zone
	m := regexp.MustCompile(`viewBox="0 0 (\d+) (\d+)"`).FindStringSubmatch(out)
	n, _ := strconv.Atoi(m[1])
	if m[1] != m[2] || (n-8-17)%4 != 0 || n < 8+21 {
		t.Fatalf("odd symbol size %v", m)
	}

	// finder pattern: the top-left 7x7 block starts with a 7 module dark bar
	if !strings.Contains(out, "M4 4h7v1h-7z") {
		t.Fatalf("no finder pattern bar in %s", out)
	}
}

func TestQRSVGDeterministicAndDistinct(t *testing.T) {
	t.Parallel()

	a, _ := qr.SVG("one", "t")
	b, _ := qr.SVG("one", "t")
	c, _ := qr.SVG("two", "t")

	if a != b || a == c {
		t.Fatal("output must depend on the text and only on it")
	}
}

func TestQRSVGEmpty(t *testing.T) {
	t.Parallel()

	if _, err := qr.SVG("", "t"); err == nil {
		t.Fatal("empty text must be refused")
	}
}
