package tui

import "github.com/charmbracelet/lipgloss"

// Styling is deliberately minimal (Design §5 doesn't ask for a themed UI,
// just a functional chat/todo surface): a handful of named lipgloss styles
// used consistently across the picker and chat views.
var (
	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, true, false, false).
			Padding(0, 1)

	modeStyle = lipgloss.NewStyle().Bold(true)

	// userMessageStyle sets a user message off from an assistant reply
	// with a lighter background block instead of a "you"/"assistant" label
	// (post-v0.1.0 addendum) — an AdaptiveColor so it reads as "slightly
	// lighter than the terminal's own background" in both a dark and a
	// light color scheme, rather than a fixed shade that only looks right
	// in one.
	userMessageStyle = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "254", Dark: "236"})

	// systemLabelStyle labels a system-style informational transcript entry
	// (post-v0.1.0 addendum: the ctrl+s skills-browser folding a skill's Body
	// into the transcript — see chatModel.showSkill) distinctly from a real
	// user/assistant message.
	systemLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))

	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)

	// greetingStyle renders the empty-transcript placeholder (post-v0.1.0
	// addendum: chatModel.refreshViewport's defaultGreeting) — dim/italic so
	// it visually reads as decoration, not a real system/assistant message.
	greetingStyle = lipgloss.NewStyle().Faint(true).Italic(true)

	todoDoneStyle = lipgloss.NewStyle().Strikethrough(true).Foreground(lipgloss.Color("8"))
)
