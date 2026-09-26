// Package mailparse turns raw RFC 5322 messages into the plain-text form
// tidymail stores and displays. HTML bodies are flattened to text: the browser
// UI never renders sender-supplied markup, which is what makes the strict
// Content-Security-Policy in the web layer possible.
package mailparse

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipElements never contribute readable text.
var skipElements = map[atom.Atom]bool{
	atom.Script: true,
	atom.Style:  true,
	atom.Head:   true,
	atom.Title:  true,
	atom.Object: true,
	atom.Embed:  true,
}

// blockElements force a line break around their content.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Table: true, atom.Tr: true,
	atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Blockquote: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
	atom.H5: true, atom.H6: true, atom.Pre: true, atom.Section: true,
	atom.Article: true, atom.Header: true, atom.Footer: true,
}

// HTMLToText renders an HTML body as readable plain text. Links are kept as
// "text <url>" so nothing is silently lost.
func HTMLToText(r io.Reader) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}
	var b strings.Builder
	walkHTML(doc, &b)
	return collapseBlankLines(b.String()), nil
}

func walkHTML(n *html.Node, b *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		writeInlineText(b, n.Data)
		return
	case html.ElementNode:
		if skipElements[n.DataAtom] {
			return
		}
		switch n.DataAtom {
		case atom.Br:
			b.WriteByte('\n')
			return
		case atom.Hr:
			b.WriteString("\n----\n")
			return
		case atom.Img:
			if alt := attr(n, "alt"); alt != "" {
				b.WriteString("[image: " + alt + "]")
			}
			return
		}
		if blockElements[n.DataAtom] {
			b.WriteByte('\n')
		}
	case html.DocumentNode, html.DoctypeNode, html.CommentNode, html.ErrorNode, html.RawNode:
		// Nothing to emit for these; children are still walked below.
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, b)
	}

	if n.Type == html.ElementNode {
		if n.DataAtom == atom.A {
			if href := attr(n, "href"); href != "" && !strings.HasPrefix(href, "#") {
				b.WriteString(" <" + href + ">")
			}
		}
		if blockElements[n.DataAtom] {
			b.WriteByte('\n')
		}
		if n.DataAtom == atom.Td || n.DataAtom == atom.Th {
			b.WriteByte('\t')
		}
	}
}

// writeInlineText appends text with runs of whitespace collapsed to a single
// space, the way a browser would lay it out.
func writeInlineText(b *strings.Builder, s string) {
	prevSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v', ' ':
			prevSpace = true
		default:
			if prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = false
			b.WriteRune(r)
		}
	}
	if prevSpace {
		b.WriteByte(' ')
	}
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// collapseBlankLines trims trailing spaces and squeezes three or more newlines
// into a blank line.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
