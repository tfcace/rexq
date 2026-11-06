package panel

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/tfcace/rexq/internal/store"
)

var (
	cText   = lipgloss.Color("#D8DEE9")
	cDim    = lipgloss.Color("#8A93A5")
	cFaint  = lipgloss.Color("#4C5566")
	cSel    = lipgloss.Color("#232A36")
	cAccent = lipgloss.Color("#88C0D0")
	cLive   = lipgloss.Color("#A3BE8C")
)

func fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

var (
	sText  = fg(cText)
	sDim   = fg(cDim)
	sFaint = fg(cFaint)
	sBold  = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sAcc   = fg(cAccent)
)

var (
	kindColor = map[string]lipgloss.Color{
		store.KindBlocker: "#F2777A", store.KindDecision: "#E5C07B", store.KindVeto: "#C792EA",
		store.KindAction: "#82AAFF", store.KindLeftover: "#6B7280",
	}
	kindGlyph = map[string]string{
		store.KindBlocker: "◆", store.KindDecision: "●", store.KindVeto: "↺",
		store.KindAction: "▲", store.KindLeftover: "·",
	}
	kindLabel = map[string]string{
		store.KindBlocker: "Blocked on you", store.KindDecision: "Decision",
		store.KindVeto: "Already done, veto?", store.KindAction: "For you to do",
		store.KindLeftover: "Leftover",
	}
)

func glyph(k string) string { return fg(kindColor[k]).Render(kindGlyph[k]) }

func chip(key, label string, c lipgloss.Color) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#11151C")).Background(c).Padding(0, 1).Render(key) +
		lipgloss.NewStyle().Foreground(cText).Background(cSel).Padding(0, 1).Render(label)
}
