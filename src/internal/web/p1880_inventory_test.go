package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"overseer/internal/web/middleware"
)

// T18.83 / T18.84 — the content security policy allows no inline or
// evaluated script, and no template contains an inline script or an
// inline event handler, so injected markup cannot execute.
func TestT1883NoInlineScript(t *testing.T) {
	csp := middleware.ContentSecurityPolicy
	scriptSrc := ""
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, "script-src") || (scriptSrc == "" && strings.HasPrefix(d, "default-src")) {
			scriptSrc = d
		}
	}
	if scriptSrc == "" || strings.Contains(scriptSrc, "'unsafe-inline'") || strings.Contains(scriptSrc, "'unsafe-eval'") {
		t.Fatalf("CSP script policy %q", scriptSrc)
	}
	inlineScript := regexp.MustCompile(`(?is)<script(\s[^>]*)?>\s*[^<\s]`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	err := filepath.WalkDir("templates", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(raw)
		for _, tag := range regexp.MustCompile(`(?is)<script[^>]*>`).FindAllString(s, -1) {
			if !strings.Contains(tag, "src=") {
				t.Errorf("%s: script tag without src: %s", p, tag)
			}
		}
		if inlineScript.MatchString(s) {
			t.Errorf("%s: inline script body", p)
		}
		if m := handler.FindString(s); m != "" {
			t.Errorf("%s: inline event handler %q", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
