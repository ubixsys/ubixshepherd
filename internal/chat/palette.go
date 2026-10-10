package chat

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// palette is every style the chat draws with. Colours adapt to the terminal's background
// (lipgloss decides light or dark once, before the program starts reading keys), and go
// away entirely when the person asks for no colour: every mark the thread and the dock
// draw is a glyph first, so nothing depends on colour, and bold and dim stay where they
// help.
type palette struct {
	color bool

	you, deskMark, tool, event, decision, err, info, head, sel lipgloss.Style
	mdHead, mdBold, mdCode, mdBlock, mdFaint                   lipgloss.Style

	// States, for event glyphs, the dock and badges: passed, failed (the loudest),
	// broken (the dock's group, and runs cut short), waiting on the person, activity,
	// and bookkeeping.
	ok, bad, broken, warn, accent, muted lipgloss.Style

	names  []lipgloss.TerminalColor // agents and lanes without a colour of their own
	agents map[string]lipgloss.TerminalColor
}

// colorEnabled is false under NO_COLOR (any non-empty value, per no-color.org) and on a
// dumb terminal.
func colorEnabled(getenv func(string) string) bool {
	return getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

func newPalette(color bool) palette {
	c := func(light, dark string) lipgloss.TerminalColor {
		if !color {
			return lipgloss.NoColor{}
		}
		return lipgloss.AdaptiveColor{Light: light, Dark: dark}
	}
	fg := func(light, dark string) lipgloss.Style { return lipgloss.NewStyle().Foreground(c(light, dark)) }
	p := palette{
		color:    color,
		you:      fg("235", "255").Bold(true),
		deskMark: fg("25", "111"),
		tool:     lipgloss.NewStyle().Faint(true),
		event:    fg("243", "244").Faint(true),
		decision: fg("136", "178"),
		err:      fg("160", "203"),
		info:     lipgloss.NewStyle().Faint(true).Italic(color),
		head:     fg("240", "245").Bold(true),
		sel:      lipgloss.NewStyle().Reverse(true),

		mdHead:  fg("235", "255").Bold(true),
		mdBold:  lipgloss.NewStyle().Bold(true),
		mdCode:  fg("130", "180"),
		mdBlock: fg("238", "250"),
		mdFaint: lipgloss.NewStyle().Faint(true),

		ok:     fg("28", "78"),
		bad:    fg("160", "203").Bold(true),
		broken: fg("160", "203"),
		warn:   fg("136", "178").Bold(true),
		accent: fg("25", "111"),
		muted:  fg("243", "244"),

		// Distinct enough to scan, quiet enough to read: blue, green, sand, steel, rose,
		// moss, sky, peach.
		names: []lipgloss.TerminalColor{
			c("25", "39"), c("28", "78"), c("130", "180"), c("61", "110"),
			c("132", "176"), c("64", "144"), c("31", "117"), c("166", "216"),
		},
		agents: map[string]lipgloss.TerminalColor{
			"claude": c("25", "111"), "copilot": c("28", "78"), "cursor": c("130", "180"),
			"opencode": c("97", "141"),
		},
	}
	if !color {
		// Without colour, the dim grey of events is just dim.
		p.event = lipgloss.NewStyle().Faint(true)
	}
	return p
}

// pal is the palette in use; the style variables below are its fields, kept as names
// so the drawing code reads plainly.
var pal palette

var (
	styleYou, styleDeskMark, styleTool, styleEvent, styleDecision lipgloss.Style
	styleError, styleInfo, styleHead, styleSel                    lipgloss.Style
	styleMdHead, styleMdBold, styleMdCode, styleMdBlock           lipgloss.Style
	styleMdFaint                                                  lipgloss.Style
)

func init() { usePalette(newPalette(colorEnabled(os.Getenv))) }

// usePalette makes p the palette every style is drawn from.
func usePalette(p palette) {
	pal = p
	styleYou, styleDeskMark, styleTool, styleEvent, styleDecision = p.you, p.deskMark, p.tool, p.event, p.decision
	styleError, styleInfo, styleHead, styleSel = p.err, p.info, p.head, p.sel
	styleMdHead, styleMdBold, styleMdCode, styleMdBlock, styleMdFaint = p.mdHead, p.mdBold, p.mdCode, p.mdBlock, p.mdFaint
}

// agentColor is an agent's own colour, or one picked from its name.
func (p palette) agentColor(name string) lipgloss.TerminalColor {
	if c, ok := p.agents[strings.ToLower(strings.TrimSpace(name))]; ok {
		return c
	}
	return p.nameColor(name)
}

// nameColor picks a colour for a name, the same one every time.
func (p palette) nameColor(name string) lipgloss.TerminalColor {
	return p.names[nameIndex(name, len(p.names))]
}

// DetectBackground asks the terminal whether its background is dark, once, before the
// program starts: asked later, the answer would race the program for the terminal's
// input. Without colour there is nothing to ask.
func DetectBackground() {
	if pal.color {
		lipgloss.HasDarkBackground()
	}
}
