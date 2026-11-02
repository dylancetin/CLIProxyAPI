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
			usage := strings.Index(body, `<script id="cpa-usage">`)
			application := strings.Index(body, `<script type="module">`)
			style := strings.Index(body, `<style id="cpa-appearance-style">`)
			usageStyleAt := strings.Index(body, `<style id="cpa-usage-style">`)
			releaseStyle := strings.Index(body, `<style>.sidebar`)
			if bootstrap < 0 || bootstrap >= application {
				t.Fatal("theme bootstrap must run before the application")
			}
			if usage <= bootstrap || usage >= application {
				t.Fatal("usage request hook must run after the theme bootstrap and before the application")
			}
			if usageStyleAt <= style {
				t.Fatal("usage styles must follow the appearance styles")
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
			for _, id := range []string{`id="cpa-appearance"`, `id="cpa-usage"`, `id="cpa-appearance-style"`, `id="cpa-usage-style"`} {
				if count := strings.Count(body, id); count != 1 {
					t.Fatalf("%s injected %d times", id, count)
				}
			}
			if !strings.Contains(body, `<div id="root"></div>`) || !strings.Contains(body, `const key = "cli-proxy-theme";`) {
				t.Fatal("application markup was lost")
			}
		})
	}
}

// Unicode lowercasing changes the byte length of some characters (the Kelvin
// sign becomes "k"), so offsets found in a lowered copy must stay aligned with
// the original page.
func TestWithAppearanceKeepsOffsetsWithMultiByteCase(t *testing.T) {
	original := []byte("<!doctype html><html><head><meta charset=\"utf-8\"><script type=\"module\">const key = \"cli-proxy-theme\"; const unit = \"\u212a\u212a\u212a\u0130\";</script><style>.release{color:red}</style></head><body><div id=\"root\"></div></body></html>")
	body := string(WithAppearance(original))
	if !strings.Contains(body, `<style>.release{color:red}</style><style id="cpa-appearance-style">`) {
		t.Fatal("appearance styles must start right after the release stylesheet")
	}
	if !strings.Contains(body, "</style></head><body>") || !strings.Contains(body, "\u212a\u212a\u212a\u0130") {
		t.Fatal("release markup was split by the injection")
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

// The assets are inlined into raw text elements, so they must not contain a
// sequence that ends the element or enters the script's escaped states.
func TestInlineAssetsCannotCloseTheirElement(t *testing.T) {
	for name, asset := range map[string][]byte{
		"appearance.js":  appearanceScript,
		"usage.js":       usageScript,
		"appearance.css": appearanceStyle,
		"usage.css":      usageStyle,
	} {
		lower := bytes.ToLower(asset)
		for _, marker := range []string{"</script", "</style", "<script", "<!--"} {
			if bytes.Contains(lower, []byte(marker)) {
				t.Errorf("%s contains %q", name, marker)
			}
		}
	}
}
