package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"

	xhtml "golang.org/x/net/html"
)

// Model output and reviewer edits end up as HTML in the catalog, so they are
// reduced to a small allow-list of formatting tags with no attributes. Text is
// always re-escaped; script and style content is dropped.

var allowedTags = map[string]bool{
	"p": true, "br": true, "ul": true, "ol": true, "li": true,
	"strong": true, "b": true, "em": true, "i": true, "h3": true, "h4": true,
}

var droppedContentTags = map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true}

// sanitizeHTML keeps only allow-listed tags (without attributes) and escapes text.
func sanitizeHTML(in string) string {
	if !strings.ContainsAny(in, "<>&") {
		return in
	}
	z := xhtml.NewTokenizer(strings.NewReader(in))
	var out strings.Builder
	skipping := ""
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return out.String()
		case xhtml.TextToken:
			if skipping == "" {
				out.WriteString(html.EscapeString(string(z.Text())))
			}
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipping != "":
			case droppedContentTags[tag]:
				if z.Token().Type == xhtml.StartTagToken {
					skipping = tag
				}
			case allowedTags[tag]:
				out.WriteString("<" + tag + ">")
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipping != "":
				if tag == skipping {
					skipping = ""
				}
			case allowedTags[tag] && tag != "br":
				out.WriteString("</" + tag + ">")
			}
		}
	}
}

const maxEditedDataBytes = 256 * 1024

// sanitizeJSONStrings validates that raw is a JSON object and sanitizes every
// string inside it (recursively). It returns the cleaned JSON.
func sanitizeJSONStrings(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maxEditedDataBytes {
		return nil, fmt.Errorf("edited data must be between 1 and %d bytes", maxEditedDataBytes)
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("edited data is not valid JSON: %w", err)
	}
	if _, ok := v.(map[string]interface{}); !ok {
		return nil, errors.New("edited data must be a JSON object")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <p> readable in the stored JSON; strings are already sanitized
	if err := enc.Encode(cleanValue(v)); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func cleanValue(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return sanitizeHTML(t)
	case []interface{}:
		for i := range t {
			t[i] = cleanValue(t[i])
		}
		return t
	case map[string]interface{}:
		for k := range t {
			t[k] = cleanValue(t[k])
		}
		return t
	default:
		return v
	}
}

// escapeLike neutralizes the wildcards of a SQL Server LIKE pattern (use with ESCAPE '\').
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `[`, `\[`)
	return r.Replace(s)
}

// stripHTML returns only the text of s (no tags, entities decoded), for fields
// that must be plain text such as a product name.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<>&") {
		return strings.TrimSpace(s)
	}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	var out strings.Builder
	skipping := ""
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return strings.TrimSpace(out.String())
		case xhtml.TextToken:
			if skipping == "" {
				out.Write(z.Text())
			}
		case xhtml.StartTagToken:
			name, _ := z.TagName()
			if droppedContentTags[string(name)] && skipping == "" {
				skipping = string(name)
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			if string(name) == skipping {
				skipping = ""
			}
		}
	}
}
