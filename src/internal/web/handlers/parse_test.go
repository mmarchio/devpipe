package handlers

import (
	"net/http"
	"testing"
)

// P17-73 — every page template parses and the assets are embedded.
func TestP1773TemplatesParse(t *testing.T) {
	c, err := New(Deps{CSRF: func(*http.Request) string { return "tok" }, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Renderer().Pages()); n < 40 {
		t.Fatalf("only %d pages parsed: %v", n, c.Renderer().Pages())
	}
	for _, a := range []string{"css/app.css", "js/live.js", "js/forms.js", "images/logo.svg"} {
		found := false
		for _, n := range c.Assets().Names() {
			found = found || n == a
		}
		if !found {
			t.Fatalf("asset %s not embedded", a)
		}
	}
}
