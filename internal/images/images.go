// Package images finds screenshots an agent message refers to.
package images

import (
	"os"
	"regexp"
	"strings"
)

// sep lists the characters that cannot be part of a path. Backticks are in
// it because agents put paths in inline code.
const sep = `\s()\[\]<>"'|` + "`"

// A path starts at the beginning of text or right after a sep character or
// one of *=:, (as in "**/a.png**", "--out=/a.png", "Shot:/a.png" and
// "/a.png,/b.png"), so "docs/tmp/a.png" does not yield "/tmp/a.png". A path
// never starts with "//", so "https://host/x.png" is not read as a local
// path either.
var pathRe = regexp.MustCompile(`(?i)(?:^|[` + sep + `*=:,])(?:file://)?(~?/[^/` + sep + `][^` + sep + `]*?\.(?:png|jpe?g|gif|webp))`)

// Max is the most screenshots kept per item.
const Max = 4

// Find returns existing image files mentioned in text, in order, without
// duplicates.
func Find(text string) []string {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var out []string
	for _, m := range pathRe.FindAllStringSubmatch(text, -1) {
		p := m[1]
		if strings.HasPrefix(p, "~/") {
			p = home + p[1:]
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out = append(out, p)
			if len(out) == Max {
				break
			}
		}
	}
	return out
}
