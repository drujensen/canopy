package tui

import "github.com/charmbracelet/lipgloss"

// Styling is deliberately minimal (Design §5 doesn't ask for a themed UI,
// just a functional chat/todo surface): a handful of named lipgloss styles
// used consistently across the picker and chat views.
var (
	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, true, false, false).
			Padding(0, 1)

	sidebarTitleStyle = lipgloss.NewStyle().Bold(true).Align(lipgloss.Center)

	modeStyle        = lipgloss.NewStyle().Bold(true)
	sidebarHintStyle = lipgloss.NewStyle().Faint(true)

	// userMessageStyle sets a user message off from an assistant reply
	// with a lighter background block instead of a "you"/"assistant" label
	// (post-v0.1.0 addendum) — an AdaptiveColor so it reads as "slightly
	// lighter than the terminal's own background" in both a dark and a
	// light color scheme, rather than a fixed shade that only looks right
	// in one.
	userMessageStyle = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "254", Dark: "236"})

	// assistantMessageStyle sets an assistant reply's own prose off with a
	// background block too (post-v0.1.0 addendum, answering "it's hard to
	// tell tool output from the actual response") — a different, subtly
	// blue-tinted shade from userMessageStyle's neutral gray, so the two
	// roles are distinguishable at a glance, not just by which side of the
	// transcript they're on. Tool call/result/error segments
	// (toolCallStyle/toolResultStyle/toolErrorStyle) deliberately get no
	// background at all, so they read as a de-emphasized aside rather than
	// part of the highlighted reply.
	assistantMessageStyle = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "#eef2fb", Dark: "#1b2430"})

	// toolCallStyle marks a "● toolname(...)" call line (indentedLines) —
	// dim but still clearly legible, distinct from both
	// assistantMessageStyle's highlighted prose and plain unstyled text.
	toolCallStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "243", Dark: "246"})

	// toolResultStyle marks a "  ⎿ ..." result line — fainter than
	// toolCallStyle's own call line, the same visual subordination Claude
	// Code gives a tool's output relative to the call that produced it.
	toolResultStyle = lipgloss.NewStyle().Faint(true)

	// toolErrorStyle marks a "  ⎿ ..." line reporting a failed tool call —
	// reuses errorStyle's own red so a failure is never mistaken for a
	// normal result, regardless of which tool it was.
	toolErrorStyle = errorStyle

	// systemLabelStyle labels a system-style informational transcript entry
	// (post-v0.1.0 addendum: the ctrl+s skills-browser folding a skill's Body
	// into the transcript — see chatModel.showSkill) distinctly from a real
	// user/assistant message.
	systemLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))

	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)

	// copyToastStyle renders the transient "Copied N lines" acknowledgment
	// (post-v0.1.0 addendum: mouse-driven text selection, chatModel.
	// copySelectionCmd) — green rather than errorStyle's red, since this is
	// confirming success, not reporting a failure.
	copyToastStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)

	// greetingStyle renders the empty-transcript placeholder (post-v0.1.0
	// addendum: chatModel.refreshViewport's defaultGreeting) — dim/italic so
	// it visually reads as decoration, not a real system/assistant message.
	greetingStyle = lipgloss.NewStyle().Faint(true).Italic(true)

	todoDoneStyle = lipgloss.NewStyle().Strikethrough(true).Foreground(lipgloss.Color("8"))
)
