package web

import (
	"net/url"
	"strings"
)

// Same-site links, so a fetched page is usable for navigation.
//
// A model reading a documentation page almost always wants the next section, and
// the alternative to this list is either a second fetch of a page it has to guess
// at, or an extractor that loses the structure entirely. Only links that stay on
// the same host are kept: an extracted page's outbound links are mostly ads,
// social buttons and unrelated sites.

// maxLinks bounds the list. A large page has hundreds of links, and the model
// cannot use hundreds.
const maxLinks = 40

func linksFrom(base *url.URL, html string) []string {
	if base == nil {
		return nil
	}
	var (
		out  []string
		seen = map[string]bool{}
	)
	for i := 0; i < len(html) && len(out) < maxLinks; {
		next := strings.Index(strings.ToLower(html[i:]), "<a ")
		if next < 0 {
			break
		}
		i += next
		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			break
		}
		tag := html[i : i+end]
		i += end + 1

		href := attrValue(tag, "href")
		if href == "" || strings.HasPrefix(href, "#") ||
			strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "javascript:") ||
			strings.HasPrefix(href, "tel:") {
			continue
		}
		ref, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			continue
		}
		resolved := base.ResolveReference(ref)
		if resolved.Host != base.Host {
			continue
		}
		resolved.Fragment = ""
		link := resolved.String()
		if seen[link] {
			continue
		}
		seen[link] = true
		out = append(out, link)
	}
	return out
}

// attrValue reads one attribute out of a tag, tolerating unquoted values.
func attrValue(tag, name string) string {
	lower := strings.ToLower(tag)
	idx := strings.Index(lower, name+"=")
	if idx < 0 {
		return ""
	}
	rest := tag[idx+len(name)+1:]
	if rest == "" {
		return ""
	}
	if rest[0] == '"' || rest[0] == '\'' {
		quote := rest[0]
		if end := strings.IndexByte(rest[1:], quote); end >= 0 {
			return decodeEntities(rest[1 : 1+end])
		}
		return ""
	}
	end := strings.IndexAny(rest, " \t\n\r>")
	if end < 0 {
		end = len(rest)
	}
	return decodeEntities(rest[:end])
}
