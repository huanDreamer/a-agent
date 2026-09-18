package web

import (
	"strconv"
	"strings"
)

// Turning HTML into text, with the standard library only.
//
// This is deliberately a bounded extractor rather than a full renderer: it drops
// the things that make a page's text unreadable (scripts, styles, comments, the
// navigation chrome), keeps the things worth reading, and keeps `<pre>`/`<code>`
// **verbatim** — a documentation page's code blocks are the reason the model asked
// for the page, and collapsing their whitespace destroys them.
//
// It is not a browser and does not try to be one. A page whose body is rendered by
// JavaScript extracts to almost nothing, and the caller reports that rather than
// pretending the page was empty.

// extract turns HTML into text.
//
// skipHead drops everything inside <head> except the title: a modern page's head
// is scripts, styles and metadata, none of which is content.
func extract(html string) (title, text string) {
	var (
		out      strings.Builder
		raw      strings.Builder // a <pre>/<code> run, kept verbatim
		titleBuf strings.Builder
		inRaw    bool
		inSkip   bool // inside script/style/head/etc
		skipTag  string
		inTitle  bool
		lastWasN bool
	)

	writeText := func(s string) {
		if inSkip {
			return
		}
		if inTitle {
			titleBuf.WriteString(s)
			return
		}
		if inRaw {
			raw.WriteString(s)
			return
		}
		out.WriteString(s)
	}
	newline := func() {
		if inSkip || inRaw {
			return
		}
		if !lastWasN {
			out.WriteByte('\n')
			lastWasN = true
		}
	}

	for i := 0; i < len(html); {
		// A comment is dropped whole, including its content: comments in a page's
		// markup are sometimes used to smuggle instructions at a model.
		if strings.HasPrefix(html[i:], "<!--") {
			if end := strings.Index(html[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
				continue
			}
			break
		}
		if html[i] != '<' {
			j := strings.IndexByte(html[i:], '<')
			if j < 0 {
				j = len(html) - i
			}
			writeText(decodeEntities(html[i : i+j]))
			if !inRaw && !inSkip {
				lastWasN = false
			}
			i += j
			continue
		}

		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			break
		}
		tag := html[i+1 : i+end]
		i += end + 1

		name, closing, selfClosing := parseTag(tag)
		switch name {
		case "script", "style", "noscript", "svg", "iframe", "template", "form", "nav", "footer", "header":
			// Navigation and chrome are what make a page's text mostly noise.
			if closing {
				if inSkip && skipTag == name {
					inSkip, skipTag = false, ""
				}
			} else if !selfClosing {
				inSkip, skipTag = true, name
			}
			continue
		case "title":
			inTitle = !closing
			continue
		case "pre", "code", "textarea":
			if closing {
				if inRaw {
					// Flush the verbatim run, fenced so the model sees it as code and
					// so the whitespace it contains is obviously deliberate.
					out.WriteString("\n```\n")
					out.WriteString(strings.Trim(raw.String(), "\n"))
					out.WriteString("\n```\n")
					raw.Reset()
					inRaw = false
					lastWasN = true
				}
				continue
			}
			if !inRaw {
				inRaw = true
				newline()
			}
			continue
		case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article", "blockquote", "table":
			newline()
			if name == "li" && !closing && !inSkip && !inRaw {
				out.WriteString("- ")
				lastWasN = false
			}
			continue
		case "td", "th":
			newline()
			continue
		}
		_ = selfClosing
	}

	// An unterminated verbatim run still has content worth keeping.
	if inRaw {
		out.WriteString("\n```\n")
		out.WriteString(strings.Trim(raw.String(), "\n"))
		out.WriteString("\n```\n")
	}

	return strings.Join(strings.Fields(titleBuf.String()), " "), tidy(out.String())
}

// parseTag reads a tag's name and shape. It tolerates the sloppiness real HTML has:
// attributes without quotes, uppercase names, stray whitespace.
func parseTag(tag string) (name string, closing, selfClosing bool) {
	tag = strings.TrimSpace(tag)
	if strings.HasPrefix(tag, "/") {
		closing = true
		tag = tag[1:]
	}
	if strings.HasSuffix(tag, "/") {
		selfClosing = true
		tag = strings.TrimSuffix(tag, "/")
	}
	end := 0
	for end < len(tag) {
		c := tag[end]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' {
			break
		}
		end++
	}
	return strings.ToLower(tag[:end]), closing, selfClosing
}

// tidy collapses the whitespace the tag stripping leaves behind: runs of spaces on
// a line, runs of blank lines, and trailing spaces.
//
// **It leaves fenced code blocks alone.** Collapsing their whitespace would make
// every sample the model fetched wrong — and a wrong sample is worse than no
// sample, because the model cannot tell that it was reformatted.
func tidy(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	inFence := false
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			// The fence line itself, and everything between two of them, is
			// verbatim.
			inFence = !inFence
			out = append(out, strings.TrimSpace(l))
			blank = 0
			continue
		}
		if inFence {
			// Trailing whitespace only: leading whitespace is the code's meaning.
			out = append(out, strings.TrimRight(l, " \t"))
			continue
		}
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// decodeEntities turns HTML entities into their characters. It handles what pages
// actually contain, including the numeric forms, rather than the whole table.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], ';')
		if end < 0 || end > 12 {
			b.WriteByte(s[i])
			i++
			continue
		}
		entity := s[i+1 : i+end]
		if r, ok := entityRune(entity); ok {
			b.WriteRune(r)
			i += end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func entityRune(entity string) (rune, bool) {
	switch entity {
	case "amp":
		return '&', true
	case "lt":
		return '<', true
	case "gt":
		return '>', true
	case "quot":
		return '"', true
	case "apos", "#39":
		return '\'', true
	case "nbsp":
		return ' ', true
	case "mdash":
		return '—', true
	case "ndash":
		return '–', true
	case "hellip":
		return '…', true
	case "times":
		return '×', true
	case "copy":
		return '©', true
	}
	if strings.HasPrefix(entity, "#x") || strings.HasPrefix(entity, "#X") {
		if n, err := strconv.ParseInt(entity[2:], 16, 32); err == nil {
			return rune(n), true
		}
		return 0, false
	}
	if strings.HasPrefix(entity, "#") {
		if n, err := strconv.ParseInt(entity[1:], 10, 32); err == nil {
			return rune(n), true
		}
	}
	return 0, false
}
