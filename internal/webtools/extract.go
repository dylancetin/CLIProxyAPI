package webtools

import (
	"bytes"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"codeberg.org/readeck/go-readability/v2/render"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// page is an extracted page, cached for paging and find.
type page struct {
	URL         string
	Title       string
	SiteName    string
	Byline      string
	Published   string
	Lang        string
	ContentType string
	Markdown    []rune
}

// Elements removed before extraction: active content, embeds and form controls.
var removedElements = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Svg: true, atom.Canvas: true,
	atom.Input: true, atom.Select: true, atom.Textarea: true, atom.Button: true, atom.Option: true,
	atom.Datalist: true, atom.Video: true, atom.Audio: true, atom.Source: true, atom.Track: true,
	atom.Map: true, atom.Link: true, atom.Base: true, atom.Frame: true, atom.Frameset: true,
	atom.Applet: true, atom.Dialog: true,
}

var (
	blankLines  = regexp.MustCompile(`\n{3,}`)
	spaceRuns   = regexp.MustCompile(`[ \t]+\n`)
	markdownCvt = converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(commonmark.WithListEndComment(false)),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	))
)

// decodeUTF8 converts a body to UTF-8 using the Content-Type charset, a BOM
// or a <meta charset>. Without any of those, valid UTF-8 is taken as UTF-8
// rather than the HTML5 windows-1252 default. Placeholder runes used for
// image alt text are removed so page text cannot forge them.
func decodeUTF8(body []byte, contentTypeHeader string) []byte {
	out := body
	if enc, _, certain := charset.DetermineEncoding(body, contentTypeHeader); certain || !utf8.Valid(body) {
		if decoded, err := io.ReadAll(enc.NewDecoder().Reader(bytes.NewReader(body))); err == nil {
			out = decoded
		}
	}
	out = bytes.ToValidUTF8(out, []byte("\uFFFD"))
	out = bytes.ReplaceAll(out, []byte(string(imgOpen)), nil)
	return bytes.ReplaceAll(out, []byte(string(imgClose)), nil)
}

// Image alt text is wrapped in private-use runes during conversion and turned
// into brackets afterwards, so the converter does not escape them.
const (
	imgOpen  = '\uE000'
	imgClose = '\uE001'
)

// extract turns a fetched body into a page.
func extract(f *fetched) *page {
	text := decodeUTF8(f.Body, f.ContentTypeHeader)
	p := &page{URL: f.FinalURL.String(), ContentType: f.ContentType}
	switch f.ContentType {
	case "text/html", "application/xhtml+xml":
		extractHTML(p, text, f.FinalURL)
	default:
		p.Markdown = []rune(cleanMarkdown(string(text)))
	}
	if p.Title == "" {
		p.Title = f.FinalURL.Hostname()
	}
	return p
}

func extractHTML(p *page, text []byte, pageURL *url.URL) {
	doc, err := html.Parse(bytes.NewReader(text))
	if err != nil {
		return
	}
	docTitle := documentTitle(doc)
	lang := documentLang(doc)
	cleanTree(doc, true)

	parser := readability.NewParser()
	article, errParse := parser.ParseAndMutate(doc, pageURL)
	content := article.Node
	if errParse != nil || content == nil || strings.TrimSpace(render.InnerText(content)) == "" {
		// Readability found nothing: fall back to the cleaned <body>.
		content = nil
		if fresh, errFresh := html.Parse(bytes.NewReader(text)); errFresh == nil {
			cleanTree(fresh, false)
			content = findElement(fresh, atom.Body)
		}
	}
	if errParse == nil {
		p.Title = singleLine(article.Title())
		p.SiteName = singleLine(article.SiteName())
		p.Byline = singleLine(article.Byline())
		if lang == "" {
			lang = article.Language()
		}
		if published, errTime := article.PublishedTime(); errTime == nil && !published.IsZero() {
			p.Published = published.UTC().Format(time.RFC3339)
		}
	}
	if p.Title == "" {
		p.Title = docTitle
	}
	p.Lang = cleanLang(lang)
	if content == nil {
		return
	}
	prepareForMarkdown(content, pageURL)
	md, err := markdownCvt.ConvertNode(content)
	if err != nil {
		return
	}
	out := strings.NewReplacer(string(imgOpen), "[", string(imgClose), "]").Replace(string(md))
	p.Markdown = []rune(cleanMarkdown(out))
}

// cleanTree removes elements that are never content or are hidden.
func cleanTree(n *html.Node, keepJSONLD bool) {
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		if removable(child, keepJSONLD) {
			n.RemoveChild(child)
		} else {
			cleanTree(child, keepJSONLD)
		}
		child = next
	}
}

func removable(n *html.Node, keepJSONLD bool) bool {
	switch n.Type {
	case html.CommentNode:
		return true
	case html.ElementNode:
	default:
		return false
	}
	if n.DataAtom == atom.Script && keepJSONLD && strings.EqualFold(strings.TrimSpace(attr(n, "type")), "application/ld+json") {
		// Structured metadata (published date, byline) that readability reads
		// and never renders.
		return false
	}
	if removedElements[n.DataAtom] {
		return true
	}
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(a.Val), "true") {
				return true
			}
		case "style":
			style := strings.ToLower(strings.Join(strings.Fields(a.Val), ""))
			if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
				return true
			}
		}
	}
	return false
}

// prepareForMarkdown makes links absolute (keeping only http(s) ones as
// links) and replaces images with their alt text.
func prepareForMarkdown(n *html.Node, base *url.URL) {
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode {
			switch child.DataAtom {
			case atom.A:
				prepareForMarkdown(child, base)
				href, ok := absoluteHTTP(attr(child, "href"), base)
				if strings.TrimSpace(render.InnerText(child)) == "" {
					// Nothing to click once images are gone: drop it.
					n.RemoveChild(child)
				} else if ok {
					child.Attr = []html.Attribute{{Key: "href", Val: href}}
					if title := attr(child, "title"); title != "" {
						child.Attr = append(child.Attr, html.Attribute{Key: "title", Val: title})
					}
				} else {
					unwrap(n, child)
				}
			case atom.Img:
				alt := singleLine(attr(child, "alt"))
				if alt != "" {
					n.InsertBefore(&html.Node{Type: html.TextNode, Data: string(imgOpen) + "Görsel: " + alt + string(imgClose)}, child)
				}
				n.RemoveChild(child)
			default:
				prepareForMarkdown(child, base)
			}
		}
		child = next
	}
}

// unwrap replaces an element with its children.
func unwrap(parent, n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		parent.InsertBefore(c, n)
		c = next
	}
	parent.RemoveChild(n)
}

func absoluteHTTP(href string, base *url.URL) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	u, err := base.Parse(href)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return u.String(), true
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func findElement(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, a); found != nil {
			return found
		}
	}
	return nil
}

func documentTitle(doc *html.Node) string {
	if t := findElement(doc, atom.Title); t != nil {
		return singleLine(render.InnerText(t))
	}
	return ""
}

// documentLang reads <html lang>, then <meta http-equiv="content-language">.
func documentLang(doc *html.Node) string {
	if h := findElement(doc, atom.Html); h != nil {
		if lang := attr(h, "lang"); lang != "" {
			return lang
		}
		if lang := attr(h, "xml:lang"); lang != "" {
			return lang
		}
	}
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Meta && strings.EqualFold(attr(n, "http-equiv"), "content-language") {
			found = attr(n, "content")
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

var langPattern = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

func cleanLang(lang string) string {
	lang = strings.TrimSpace(strings.Split(lang, ",")[0])
	lang = strings.ReplaceAll(lang, "_", "-")
	if !langPattern.MatchString(lang) {
		return ""
	}
	return lang
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cleanMarkdown normalizes line endings, trims trailing spaces and collapses
// runs of blank lines.
func cleanMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = spaceRuns.ReplaceAllString(s, "\n")
	s = blankLines.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}
