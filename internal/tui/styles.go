package tui

import "github.com/charmbracelet/lipgloss"

// Styling is deliberately minimal (Design §5 doesn't ask for a themed UI,
// just a functional chat/approval/todo/mode surface): a handful of named
// lipgloss styles used consistently across the picker and chat views.
var (
	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, true, false, false).
			Padding(0, 1)

	modeStyle = lipgloss.NewStyle().Bold(true)

	userLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))

	assistantLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))

	// systemLabelStyle labels a system-style informational transcript entry
	// (post-v0.1.0 addendum: the ctrl+s skills-browser folding a skill's Body
	// into the transcript — see chatModel.showSkill) distinctly from a real
	// user/assistant message.
	systemLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))

	approvalStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("3")).
			Padding(0, 1)

	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)

	todoDoneStyle = lipgloss.NewStyle().Strikethrough(true).Foreground(lipgloss.Color("8"))
)
