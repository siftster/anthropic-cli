package tui

import (
	"regexp"
	"strings"

	"github.com/muesli/reflow/wrap"
)

var (
	mdFence   = regexp.MustCompile("^\\s*```")
	mdHeading = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	mdCode    = regexp.MustCompile("`([^`\n]+)`")
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	// Asterisk forms only: underscores would mangle __init__ and snake_case.
	// The opening * must start a word so a*b*c passes through.
	mdItalic = regexp.MustCompile(`(^|[\s(])\*([^*\s][^*\n]*)\*`)
	mdLink   = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
)

// markdown renders the little markdown agent prose actually uses — emphasis,
// code spans, links, headings, fenced blocks — and wraps to width. Lists keep
// their own markers; anything fancier prints as typed.
func markdown(text string, width int) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if mdFence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			for _, wrapped := range strings.Split(wrap.String("  "+line, max(width, 1)), "\n") {
				out = append(out, stDim.Render(wrapped))
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapLines(inlineMarkdown(line), width)...)
	}
	return out
}

// inlineMarkdown styles one line outside a fence. A heading is bolded whole.
func inlineMarkdown(line string) string {
	if heading := mdHeading.FindStringSubmatch(line); heading != nil {
		return stBold.Render(heading[1])
	}
	// Styling the template works because ANSI sequences never contain '$'.
	line = mdCode.ReplaceAllString(line, stSpan.Render("${1}"))
	line = mdBold.ReplaceAllString(line, stBold.Render("${1}"))
	// Not the template trick: lipgloss emits underline per rune, which would shred "${2}".
	line = mdItalic.ReplaceAllStringFunc(line, func(match string) string {
		groups := mdItalic.FindStringSubmatch(match)
		return groups[1] + stEm.Render(groups[2])
	})
	return mdLink.ReplaceAllString(line, "${1}"+stDim.Render(" (${2})"))
}
