package webui

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var rootURLAttribute = regexp.MustCompile(`\s(?:href|src|fx-action|action)="(/[^\"]*)"`)

// injectBase scopes root-relative template URLs without changing scripts or
// literal content. The signer bridge scopes its requests with localPath.
func injectBase(source, base string) string {
	if base == "" {
		return source
	}
	var out strings.Builder
	out.Grow(len(source))
	tokens := html.NewTokenizer(strings.NewReader(source))
	for {
		kind := tokens.Next()
		raw := string(tokens.Raw())
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			raw = rootURLAttribute.ReplaceAllStringFunc(raw, func(attribute string) string {
				start := strings.IndexByte(attribute, '"') + 1
				path := attribute[start : len(attribute)-1]
				if strings.HasPrefix(path, "//") || path == base || strings.HasPrefix(path, base+"/") {
					return attribute
				}
				return attribute[:start] + base + path + `"`
			})
		}
		out.WriteString(raw)
		if kind == html.ErrorToken {
			return out.String()
		}
	}
}
