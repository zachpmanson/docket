package mail

import "testing"

// TestHTMLToTextDecodesCharacterReferences pins the flag-independent half of
// the body contract: the field is documented as plain text, and "&amp;" is
// not plain text.
func TestHTMLToTextDecodesCharacterReferences(t *testing.T) {
	cases := []struct{ name, markup, want string }{{
		name:   "named and numeric references",
		markup: `<p>Rates &amp; charges&nbsp;apply. 5 &lt; 6 &gt; 4. &quot;q&quot; &#39;a&#39;</p>`,
		want:   "Rates & charges apply. 5 < 6 > 4. \"q\" 'a'",
	}, {
		name: "a reference inside a link's text",
		markup: `<a href="https://app.example.com/r?id=2547678&amp;tab=notes">` +
			`costs &amp; timing</a>`,
		want: "costs & timing",
	}, {
		// Decoding before the tag strip would turn this into "<div>" and the
		// strip would then delete it, removing text the message displayed.
		name:   "escaped markup the sender meant as text",
		markup: `<p>write &lt;div&gt; to open a div</p>`,
		want:   "write <div> to open a div",
	}, {
		// HTML5 resolves a handful of legacy names without their semicolon,
		// and browsers do, so the recipient saw an ampersand here too.
		name:   "a reference without its semicolon, as a browser reads it",
		markup: `<p>a &amp b</p>`,
		want:   "a & b",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToText(tc.markup); got != tc.want {
				t.Errorf("htmlToText(%q) = %q, want %q", tc.markup, got, tc.want)
			}
		})
	}
}

func TestHTMLToTextDropsScriptAndStyleContent(t *testing.T) {
	// Stripping tags alone leaves the CSS between them, and in HTML-only mail
	// that block is often longer than the message.
	markup := `<html><head><style type="text/css">.x{color:#fff}</style></head>` +
		`<body><script>var t=1;</script><p>Meeting moved to 3pm.</p></body></html>`

	if got := htmlToText(markup); got != "Meeting moved to 3pm." {
		t.Errorf("htmlToText = %q, want just the prose", got)
	}
}

// TestPlainPartKeepsLiteralEntityText is why the decode belongs in the
// conversion and not in the caller. A caller unescaping the body field cannot
// tell markup it should decode from a plain part whose author meant those
// five characters.
func TestPlainPartKeepsLiteralEntityText(t *testing.T) {
	body := "In HTML an ampersand is written &amp; and a space is &nbsp;.\nThe URL ends in ?a=1&amp;b=2\n"

	msg, err := readFixture(t, body, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Body != body {
		t.Errorf("body = %q, want the plain part byte for byte: %q", msg.Body, body)
	}
}
