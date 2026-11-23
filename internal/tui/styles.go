package tui

import "github.com/charmbracelet/lipgloss"

// ─── Palette ─────────────────────────────────────────────────────────────────
//
// Adaptive colors keep the operator console readable on both light and dark
// terminals. The accent is deliberately strong: a selection that only differs
// by a near-black background is invisible on a dark theme.

var (
	colorBase     = lipgloss.AdaptiveColor{Light: "#f8fafc", Dark: "#0b0d10"}
	colorSurface  = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#151a23"}
	colorSurface2 = lipgloss.AdaptiveColor{Light: "#f1f4f8", Dark: "#1b2130"}
	colorOverlay  = lipgloss.AdaptiveColor{Light: "#d7dce3", Dark: "#303849"}
	colorText     = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#e8eaf1"}
	colorSubtext  = lipgloss.AdaptiveColor{Light: "#5b6578", Dark: "#9aa4b6"}
	colorFaint    = lipgloss.AdaptiveColor{Light: "#94a0b4", Dark: "#667085"}
	colorLavender = lipgloss.AdaptiveColor{Light: "#4f46e5", Dark: "#a5b4fc"} // brand accent
	colorGreen    = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#34d399"}
	colorPeach    = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#fbbf24"}
	colorRed      = lipgloss.AdaptiveColor{Light: "#b91c1c", Dark: "#f87171"}
	colorBlue     = lipgloss.AdaptiveColor{Light: "#1d4ed8", Dark: "#93c5fd"}
	colorMauve    = lipgloss.AdaptiveColor{Light: "#6d28d9", Dark: "#c4b5fd"}
	colorYellow   = lipgloss.AdaptiveColor{Light: "#a16207", Dark: "#fde68a"}
	colorTeal     = lipgloss.AdaptiveColor{Light: "#0f766e", Dark: "#5eead4"}
	colorOnAccent = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#0b0d10"}
	colorAccentBg = lipgloss.AdaptiveColor{Light: "#e0e7ff", Dark: "#312e81"}
	colorWarnBg   = lipgloss.AdaptiveColor{Light: "#fffbeb", Dark: "#2a2108"}
	colorErrBg    = lipgloss.AdaptiveColor{Light: "#fef2f2", Dark: "#2b0f12"}
	colorOkBg     = lipgloss.AdaptiveColor{Light: "#ecfdf5", Dark: "#052e2b"}
)

// ─── Frame styles ────────────────────────────────────────────────────────────

var (
	appStyle = lipgloss.NewStyle().
			Foreground(colorText).
			Padding(1, 2)

	brandStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorLavender)

	brandNameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorText)

	versionStyle = lipgloss.NewStyle().
			Foreground(colorFaint)

	scopeStyle = lipgloss.NewStyle().
			Foreground(colorLavender).
			Bold(true)

	scopeHintStyle = lipgloss.NewStyle().
			Foreground(colorPeach)

	ruleStyle = lipgloss.NewStyle().
			Foreground(colorOverlay)

	pageTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorText)

	pageSubStyle = lipgloss.NewStyle().
			Foreground(colorSubtext)

	helpStyle = lipgloss.NewStyle().
			Foreground(colorFaint)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorRed).
			Bold(true).
			Background(colorErrBg).
			Padding(0, 1)

	copyFeedbackStyle = lipgloss.NewStyle().
				Foreground(colorGreen).
				Bold(true).
				Background(colorOkBg).
				Padding(0, 1)

	updateBannerStyle = lipgloss.NewStyle().
				Foreground(colorYellow).
				Bold(true).
				Padding(0, 1)
)

// ─── Dashboard styles ────────────────────────────────────────────────────────

var (
	statNumberStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorLavender)

	statLabelStyle = lipgloss.NewStyle().
			Foreground(colorSubtext)

	statCardStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(colorOverlay).
			Background(colorSurface).
			Padding(0, 2)

	menuItemStyle = lipgloss.NewStyle().
			Foreground(colorText)

	menuSelectedStyle = lipgloss.NewStyle().
				Foreground(colorOnAccent).
				Background(colorLavender).
				Bold(true)

	menuIconStyle = lipgloss.NewStyle().
			Foreground(colorMauve)

	menuDescStyle = lipgloss.NewStyle().
			Foreground(colorFaint)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorSubtext).
			MarginTop(1).
			MarginBottom(1)
)

// ─── List styles ─────────────────────────────────────────────────────────────

var (
	listItemStyle = lipgloss.NewStyle().
			Foreground(colorText)

	listSelectedStyle = lipgloss.NewStyle().
				Foreground(colorOnAccent).
				Background(colorLavender).
				Bold(true)

	typeBadgeStyle = lipgloss.NewStyle().
			Foreground(colorPeach).
			Bold(true)

	stateWarningBadgeStyle = lipgloss.NewStyle().
				Foreground(colorYellow).
				Bold(true)

	idStyle = lipgloss.NewStyle().
		Foreground(colorBlue)

	timestampStyle = lipgloss.NewStyle().
			Foreground(colorSubtext)

	projectStyle = lipgloss.NewStyle().
			Foreground(colorYellow)

	contentPreviewStyle = lipgloss.NewStyle().
				Foreground(colorSubtext).
				PaddingLeft(4)
)

// ─── Detail styles ───────────────────────────────────────────────────────────

var (
	sectionHeadingStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorMauve).
				MarginTop(1).
				MarginBottom(1)

	detailContentStyle = lipgloss.NewStyle().
				Foreground(colorText).
				PaddingLeft(2)

	detailLabelStyle = lipgloss.NewStyle().
				Foreground(colorSubtext).
				Width(14).
				Align(lipgloss.Right).
				PaddingRight(1)

	detailValueStyle = lipgloss.NewStyle().
				Foreground(colorText)
)

// ─── Timeline styles ─────────────────────────────────────────────────────────

var (
	timelineFocusStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(colorLavender).
				Padding(0, 1)

	timelineItemStyle = lipgloss.NewStyle().
				Foreground(colorSubtext).
				PaddingLeft(2)

	timelineConnectorStyle = lipgloss.NewStyle().
				Foreground(colorOverlay)
)

// ─── Search styles ───────────────────────────────────────────────────────────

var (
	searchInputStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.RoundedBorder()).
				BorderForeground(colorLavender).
				Foreground(colorText).
				Padding(0, 1).
				MarginBottom(1)

	searchHighlightStyle = lipgloss.NewStyle().
				Foreground(colorTeal).
				Bold(true)

	noResultsStyle = lipgloss.NewStyle().
			Foreground(colorSubtext).
			Italic(true)
)
