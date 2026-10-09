package driver

import (
	"fmt"
	"regexp"
	"strings"
)

var sshdDirectivePatterns = map[string]*regexp.Regexp{}

// normalizeSSHDirective rewrites every line of an sshd_config that sets
// `directive` (regardless of leading whitespace, comments or case) into a
// single canonical `directive value` line, appending one when absent.
// AGT-F6: the legacy sed+grep pair missed indented or commented directives,
// so the appended line was shadowed by an earlier effective directive.
func normalizeSSHDirective(config, directive, value string) string {
	pattern := sshdDirectivePatterns[directive]
	if pattern == nil {
		// Allow leading whitespace, an optional comment marker and optional
		// whitespace before the directive keyword itself.
		pattern = regexp.MustCompile(`(?im)^[\t ]*#?[\t ]*` + regexp.QuoteMeta(directive) + `[\t ]+[^\n]*`)
		sshdDirectivePatterns[directive] = pattern
	}
	replaced := pattern.ReplaceAllString(config, fmt.Sprintf("%s %s", directive, value))
	if strings.Contains(replaced, fmt.Sprintf("\n%s %s", directive, value)) || strings.HasPrefix(strings.TrimLeft(replaced, "\n"), fmt.Sprintf("%s %s", directive, value)) {
		// Count occurrences of the canonical line; collapse duplicates.
		lines := strings.Split(replaced, "\n")
		canonical := fmt.Sprintf("%s %s", directive, value)
		out := make([]string, 0, len(lines))
		seen := false
		for _, line := range lines {
			if strings.TrimSpace(line) == canonical {
				if seen {
					continue
				}
				seen = true
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	return strings.ReplaceAll(replaced, "#"+directive, directive) + fmt.Sprintf("%s %s\n", directive, value)
}
