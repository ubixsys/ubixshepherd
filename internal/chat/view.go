package chat

import (
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/ubixsys/ubixshepherd/internal/api"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

func nameIndex(name string, n int) int {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() % uint32(n))
}

func styleAgent(name string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(pal.agentColor(name))
}

func styleLane(name string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(pal.nameColor("lane:" + name))
}

// renderLine is one thread entry as it is printed to the terminal's scrollback (and shown
// in the transcript), wrapped to width.
func renderLine(l Line, width int) string {
	w := width - 2
	if w < 10 {
		w = 10
	}
	switch l.Kind {
	case KindYou:
		return hang(styleYou.Render("›")+" ", "  ", styleLines(styleYou, wrapText(l.Text, w)))
	case KindDesk:
		return hang(styleDeskMark.Render("●")+" ", "  ", renderMarkdown(l.Text, w))
	case KindTool:
		return hang(styleTool.Render("  → "), "    ", styleLines(styleTool, wrapText(l.Text, w-2)))
	case KindEvent:
		return hang(eventGlyph(l.Event)+" ", "  ", styleLines(styleEvent, tintAgents(wrapText(l.Text, w))))
	case KindDecision:
		return hang(eventGlyph(api.EventDecisionAsked)+" ", "  ", styleLines(styleDecision, wrapText(l.Text, w)))
	case KindError:
		return hang(styleError.Render("!")+" ", "  ", styleLines(styleError, wrapText(l.Text, w)))
	}
	return hang("  ", "  ", styleLines(styleInfo, wrapText(l.Text, w)))
}

// hang prefixes the first line of body with first and the others with rest.
func hang(first, rest, body string) string {
	lines := strings.Split(body, "\n")
	for i := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		lines[i] = p + lines[i]
	}
	return strings.Join(lines, "\n")
}

// styleLines styles each line on its own: a style over several lines would pad them all
// to the widest, and trailing spaces get in the way of copying from the terminal.
func styleLines(st lipgloss.Style, s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = st.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// wrapText wraps each line to width at word boundaries, breaking words that do not fit.
func wrapText(text string, width int) string {
	if width < 1 {
		width = 1
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = ansi.Wrap(strings.TrimRight(l, " \t"), width, "")
	}
	return strings.Join(lines, "\n")
}

// fit cuts a line to width, for the live region, which must never wrap.
func fit(s string, width int) string {
	if width <= 0 || ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

func clipTo(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(rs[:n-1]) + "…"
}

// tintAgents colors known agent names inside a plain line (panel / event text).
func tintAgents(s string) string {
	for _, a := range []string{"claude", "copilot", "cursor", "opencode"} {
		if i := indexFold(s, a); i >= 0 {
			before, mid, after := s[:i], s[i:i+len(a)], s[i+len(a):]
			return before + styleAgent(a).Render(mid) + tintAgents(after)
		}
	}
	return s
}

func indexFold(s, sub string) int {
	ls, lsub := strings.ToLower(s), strings.ToLower(sub)
	i := strings.Index(ls, lsub)
	if i < 0 {
		return -1
	}
	// Prefer whole-word matches.
	if i > 0 {
		prev := rune(s[i-1])
		if unicode.IsLetter(prev) || unicode.IsDigit(prev) {
			rest := indexFold(s[i+1:], sub)
			if rest < 0 {
				return -1
			}
			return i + 1 + rest
		}
	}
	end := i + len(sub)
	if end < len(s) {
		next := rune(s[end])
		if unicode.IsLetter(next) || unicode.IsDigit(next) {
			rest := indexFold(s[i+1:], sub)
			if rest < 0 {
				return -1
			}
			return i + 1 + rest
		}
	}
	return i
}

func short(state string) string {
	switch state {
	case store.RunRunning:
		return "running"
	case store.RunSucceeded:
		return "done"
	}
	return state
}

func fmtUSD(usd, budget float64, day string) string {
	if day == "" {
		return ""
	}
	s := fmt.Sprintf("  ·  $%.2f today", usd)
	if budget > 0 {
		s += fmt.Sprintf(" of $%.0f", budget)
	}
	return s
}
