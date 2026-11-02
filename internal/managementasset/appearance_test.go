package managementasset

import (
	"bytes"
	"strings"
	"testing"
)

func TestWithAppearance(t *testing.T) {
	for _, head := range []string{"head", "HEAD"} {
		t.Run(head, func(t *testing.T) {
			original := []byte(`<!doctype html><html><` + head + `><meta charset="utf-8"><script type="module">const key = "cli-proxy-theme";</script><style>.sidebar {color:red}</style></` + head + `><body><div id="root"></div></body></html>`)
			before := bytes.Clone(original)
			got := WithAppearance(original)
			if !bytes.Equal(original, before) {
				t.Fatal("WithAppearance modified the downloaded asset")
			}
			body := string(got)
			bootstrap := strings.Index(body, `<script id="cpa-appearance">`)
			application := strings.Index(body, `<script type="module">`)
			style := strings.Index(body, `<style id="cpa-appearance-style">`)
			releaseStyle := strings.Index(body, `<style>.sidebar`)
			if bootstrap < 0 || bootstrap >= application {
				t.Fatal("theme bootstrap must run before the application")
			}
			if strings.Index(body, `<meta charset="utf-8">`) >= bootstrap {
				t.Fatal("charset metadata must precede the appearance script")
			}
			if style <= releaseStyle {
				t.Fatal("appearance styles must follow the release styles")
			}
			if !bytes.Equal(got, WithAppearance(got)) {
				t.Fatal("appearance was applied more than once")
			}
			if !strings.Contains(body, `<div id="root"></div>`) || !strings.Contains(body, `const key = "cli-proxy-theme";`) {
				t.Fatal("application markup was lost")
			}
		})
	}
}

func TestWithAppearanceLeavesUnknownPanelsUntouched(t *testing.T) {
	for _, page := range []string{
		"",
		"<html>custom management app</html>",
		"<html><head></head><body>custom panel</body></html>",
		"cli-proxy-theme without a head",
		"<head>cli-proxy-theme without a closing head",
		"</head><head>cli-proxy-theme",
	} {
		if got := WithAppearance([]byte(page)); string(got) != page {
			t.Errorf("modified unsupported panel %q", page)
		}
	}
}
