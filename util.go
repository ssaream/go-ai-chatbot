package main

import "strings"

func uniqueFields(in []Field) []Field {
	seen := map[Field]bool{}
	out := []Field{}
	for _, f := range in {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizePhone(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "+") {
		var b strings.Builder
		b.WriteByte('+')
		for _, ch := range s[1:] {
			if ch >= '0' && ch <= '9' {
				b.WriteRune(ch)
			}
		}
		return b.String()
	}
	var b strings.Builder
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func firstNonEmpty(a, b string) string {
	a = strings.TrimSpace(a)
	if a != "" {
		return a
	}
	return strings.TrimSpace(b)
}
