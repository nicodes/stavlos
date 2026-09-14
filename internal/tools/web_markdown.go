package tools

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// HTML → markdown: a small readability pass for web_fetch (see web_fetch.go).

// htmlToMarkdown is a small readability pass: it takes <main> or <article>
// when the page has one, drops chrome (nav, header, footer, aside, scripts,
// styles, forms' controls), and renders headings, paragraphs, lists, links,
// code and tables as markdown. Links are resolved against base.
func htmlToMarkdown(src []byte, base *url.URL) string {
	doc, err := html.Parse(bytes.NewReader(src))
	if err != nil {
		return string(src)
	}
	root := doc
	if body := findNode(doc, "body"); body != nil {
		root = body
	}
	for _, tag := range []string{"main", "article"} {
		if n := findNode(root, tag); n != nil {
			root = n
			break
		}
	}
	c := &mdConverter{base: base}
	c.walk(root)
	out := string(c.out)
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}

func findNode(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findNode(c, tag); f != nil {
			return f
		}
	}
	return nil
}

type mdConverter struct {
	out   []byte // the markdown so far; appended to, trimmed in place, never rebuilt
	base  *url.URL
	pre   int // inside <pre>: whitespace kept
	quote int // inside <blockquote>: lines prefixed
	list  []listState
}

type listState struct {
	ordered bool
	n       int
}

var skipTags = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true, "nav": true, "header": true, "footer": true, "aside": true, "iframe": true, "button": true, "input": true, "select": true, "textarea": true, "head": true}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (c *mdConverter) text(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			rec(k)
		}
	}
	rec(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

// mdOverflow is where the converter stops reading: a little past the page
// cap, since fetchPage cuts at webMaxMarkdown anyway.
const mdOverflow = webMaxMarkdown + 4096

// write appends to the output up to the cap; the rest of the page is
// never shown, so it is never converted.
func (c *mdConverter) write(s string) {
	if len(c.out) >= mdOverflow {
		return
	}
	c.out = append(c.out, s...)
	if len(c.out) > mdOverflow {
		c.out = c.out[:mdOverflow]
	}
}

// endsWith reports whether the output so far ends with s.
func (c *mdConverter) endsWith(s string) bool { return bytes.HasSuffix(c.out, []byte(s)) }

func (c *mdConverter) block(s string) {
	if len(c.out) > 0 && !c.endsWith("\n\n") {
		if c.endsWith("\n") {
			c.write("\n")
		} else {
			c.write("\n\n")
		}
	}
	if c.quote > 0 {
		s = "> " + strings.ReplaceAll(s, "\n", "\n> ")
	}
	c.write(s)
	c.write("\n\n")
}

func (c *mdConverter) walk(n *html.Node) {
	if len(c.out) >= mdOverflow {
		return // past the cap: the rest of the page is never shown
	}
	switch n.Type {
	case html.TextNode:
		if c.pre > 0 {
			c.write(n.Data)
			return
		}
		t := strings.Join(strings.Fields(n.Data), " ")
		if t == "" {
			if strings.ContainsAny(n.Data, " \n\t") && len(c.out) > 0 && !c.endsWith(" ") && !c.endsWith("\n") {
				c.write(" ")
			}
			return
		}
		if strings.HasPrefix(n.Data, " ") || strings.HasPrefix(n.Data, "\n") {
			if len(c.out) > 0 && !c.endsWith(" ") && !c.endsWith("\n") {
				c.write(" ")
			}
		}
		c.write(t)
		if strings.HasSuffix(n.Data, " ") || strings.HasSuffix(n.Data, "\n") {
			c.write(" ")
		}
		return
	case html.ElementNode:
	default:
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.walk(k)
		}
		return
	}
	if skipTags[n.Data] {
		return
	}
	switch n.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(n.Data[1] - '0')
		if t := c.text(n); t != "" {
			c.block(strings.Repeat("#", level) + " " + t)
		}
	case "p", "div", "section", "article", "main", "figure", "figcaption", "details", "summary", "dd", "dt", "address":
		c.flushLine()
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.walk(k)
		}
		c.flushLine()
		if n.Data == "p" || n.Data == "section" || n.Data == "article" {
			c.write("\n")
		}
	case "br":
		c.write("\n")
	case "hr":
		c.block("---")
	case "pre":
		code := c.rawText(n)
		lang := ""
		if cd := findNode(n, "code"); cd != nil {
			for _, cls := range strings.Fields(attr(cd, "class")) {
				if strings.HasPrefix(cls, "language-") {
					lang = strings.TrimPrefix(cls, "language-")
				}
			}
		}
		c.block("```" + lang + "\n" + strings.TrimRight(code, "\n") + "\n```")
	case "code", "kbd", "samp":
		if t := c.rawText(n); t != "" {
			c.write("`" + strings.ReplaceAll(strings.TrimSpace(t), "\n", " ") + "`")
		}
	case "strong", "b":
		if t := c.text(n); t != "" {
			c.write("**" + t + "**")
		}
	case "em", "i":
		if t := c.text(n); t != "" {
			c.write("*" + t + "*")
		}
	case "a":
		t := c.text(n)
		href := attr(n, "href")
		if c.base != nil && href != "" {
			if ref, err := url.Parse(href); err == nil {
				href = c.base.ResolveReference(ref).String()
			}
		}
		switch {
		case t == "" && href == "":
		case href == "" || strings.HasPrefix(href, "javascript:") || t == href:
			c.write(t)
		case t == "":
			c.write(href)
		default:
			c.write("[" + t + "](" + href + ")")
		}
	case "img":
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			c.write("[image: " + alt + "]")
		}
	case "ul", "ol":
		c.flushLine()
		c.list = append(c.list, listState{ordered: n.Data == "ol"})
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.walk(k)
		}
		c.list = c.list[:len(c.list)-1]
		if len(c.list) == 0 {
			c.write("\n")
		}
	case "li":
		c.flushLine()
		indent := strings.Repeat("  ", max(len(c.list)-1, 0))
		marker := "- "
		if len(c.list) > 0 && c.list[len(c.list)-1].ordered {
			c.list[len(c.list)-1].n++
			marker = fmt.Sprintf("%d. ", c.list[len(c.list)-1].n)
		}
		c.write(indent + marker)
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.walk(k)
		}
		c.flushLine()
	case "blockquote":
		c.flushLine()
		c.quote++
		start := len(c.out)
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.walk(k)
		}
		c.flushLine()
		c.quote--
		// re-prefix what the children wrote (blocks already carry ">"; plain
		// lines do not): only the quote's own text is rewritten.
		inner := string(c.out[start:])
		c.out = c.out[:start]
		var lines []string
		for _, l := range strings.Split(strings.TrimRight(inner, "\n"), "\n") {
			if !strings.HasPrefix(l, ">") && strings.TrimSpace(l) != "" {
				l = "> " + l
			}
			lines = append(lines, l)
		}
		c.write(strings.Join(lines, "\n") + "\n\n")
	case "table":
		c.flushLine()
		var rows []string
		for _, tr := range findAll(n, "tr") {
			var cells []string
			for k := tr.FirstChild; k != nil; k = k.NextSibling {
				if k.Type == html.ElementNode && (k.Data == "td" || k.Data == "th") {
					cells = append(cells, c.text(k))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
			}
		}
		if len(rows) > 0 {
			if len(rows) > 1 {
				cols := strings.Count(rows[0], "|") - 1
				rows = append([]string{rows[0], "|" + strings.Repeat(" --- |", cols)}, rows[1:]...)
			}
			c.block(strings.Join(rows, "\n"))
		}
	default:
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			c.walk(k)
		}
	}
}

// rawText is the text of n with whitespace kept (code).
func (c *mdConverter) rawText(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.Data == "br" {
			sb.WriteString("\n")
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			rec(k)
		}
	}
	rec(n)
	return sb.String()
}

// flushLine ends the current line if something is on it, dropping its
// trailing spaces in place.
func (c *mdConverter) flushLine() {
	if len(c.out) > 0 && c.out[len(c.out)-1] != '\n' {
		c.out = append(bytes.TrimRight(c.out, " "), '\n')
	}
}

func findAll(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			rec(k)
		}
	}
	rec(n)
	return out
}
