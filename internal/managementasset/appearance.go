package managementasset

import (
	"bytes"
	_ "embed"
)

// Keep the appearance separate from the downloaded release so its digest stays
// verifiable and automatic panel updates cannot replace the local styling.
//
//go:embed appearance.js
var appearanceScript []byte

//go:embed appearance.css
var appearanceStyle []byte

// WithAppearance applies the local appearance to compatible CPAMC releases.
// Unknown/custom panels are returned untouched. The early script initializes
// the existing theme store before the application runs; the final stylesheet
// overrides release styles without modifying the application's controls.
func WithAppearance(page []byte) []byte {
	if !bytes.Contains(page, []byte("cli-proxy-theme")) || bytes.Contains(page, []byte(`id="cpa-appearance"`)) {
		return page
	}
	lower := asciiLower(page)
	headStart := bytes.Index(lower, []byte("<head>"))
	headEnd := bytes.Index(lower, []byte("</head>"))
	if headStart < 0 || headEnd < headStart {
		return page
	}
	headStart += len("<head>")
	// Leave charset metadata at the beginning of the document, while still
	// initializing the theme before any application script can execute.
	if script := bytes.Index(lower[headStart:headEnd], []byte("<script")); script >= 0 {
		headStart += script
	} else {
		headStart = headEnd
	}
	var result bytes.Buffer
	result.Grow(len(page) + len(appearanceScript) + len(appearanceStyle) + 100)
	result.Write(page[:headStart])
	result.WriteString(`<script id="cpa-appearance">`)
	result.Write(appearanceScript)
	result.WriteString("</script>")
	result.Write(page[headStart:headEnd])
	result.WriteString(`<style id="cpa-appearance-style">`)
	result.Write(appearanceStyle)
	result.WriteString("</style>")
	result.Write(page[headEnd:])
	return result.Bytes()
}

// asciiLower folds only ASCII letters so every offset found in the result is
// also valid in page; Unicode case mapping can change UTF-8 byte lengths.
func asciiLower(page []byte) []byte {
	lower := make([]byte, len(page))
	for i, c := range page {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	return lower
}
