package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
)

// ---------------------------------------------------------------------
// formatCost
// ---------------------------------------------------------------------

func TestFormatCost(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{name: "whole dollar amount drops trailing .00", in: 3, want: "3"},
		{name: "half dollar keeps one decimal", in: 0.5, want: "0.5"},
		{name: "two decimals kept when both non-zero", in: 2.25, want: "2.25"},
		{name: "zero renders as bare 0", in: 0, want: "0"},
		{name: "large whole amount", in: 1234, want: "1234"},
		{name: "rounds to two decimal places", in: 1.005, want: "1"}, // %.2f rounds 1.005 -> "1.00" (float64 repr), trimmed to "1"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCost(tc.in)
			if got != tc.want {
				t.Errorf("formatCost(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// modelPickerItem
// ---------------------------------------------------------------------

func TestModelPickerItem_Description(t *testing.T) {
	t.Run("both costs zero renders blank, not $0/$0", func(t *testing.T) {
		item := modelPickerItem{name: "local-model"}
		if got := item.Description(); got != "" {
			t.Errorf("Description() = %q, want empty string for an unconfigured/self-hosted model", got)
		}
	})

	t.Run("both costs set", func(t *testing.T) {
		item := modelPickerItem{name: "sonnet", inputCostPerMillion: 3, outputCostPerMillion: 15}
		want := "$3 in / $15 out"
		if got := item.Description(); got != want {
			t.Errorf("Description() = %q, want %q", got, want)
		}
	})

	t.Run("only input cost set", func(t *testing.T) {
		item := modelPickerItem{name: "weird", inputCostPerMillion: 1.5}
		want := "$1.5 in / $0 out"
		if got := item.Description(); got != want {
			t.Errorf("Description() = %q, want %q", got, want)
		}
	})
}

func TestModelPickerItem_TitleAndFilterValue(t *testing.T) {
	item := modelPickerItem{name: "haiku"}
	if item.Title() != "haiku" {
		t.Errorf("Title() = %q, want %q", item.Title(), "haiku")
	}
	if item.FilterValue() != "haiku" {
		t.Errorf("FilterValue() = %q, want %q", item.FilterValue(), "haiku")
	}
}

// ---------------------------------------------------------------------
// historyPickerItem
// ---------------------------------------------------------------------

func TestHistoryPickerItem_Title_UsesGeneratedTitleWhenPresent(t *testing.T) {
	when := time.Date(2026, 3, 5, 14, 30, 0, 0, time.UTC)
	item := historyPickerItem{chatID: "c1", title: "Debugging the parser", agentName: "assistant", updatedAt: when}
	if got := item.Title(); got != "Debugging the parser" {
		t.Errorf("Title() = %q, want %q", got, "Debugging the parser")
	}
}

func TestHistoryPickerItem_Title_FallsBackToDateWhenEmpty(t *testing.T) {
	when := time.Date(2026, 3, 5, 14, 30, 0, 0, time.UTC)
	item := historyPickerItem{chatID: "c1", title: "", agentName: "assistant", updatedAt: when}
	got := item.Title()
	if got == "" {
		t.Fatal("Title() must fall back to a formatted date, not stay empty")
	}
	want := when.Local().Format(historyDateFormat)
	if got != want {
		t.Errorf("Title() = %q, want formatted date %q", got, want)
	}
}

func TestHistoryPickerItem_Description_AlwaysShowsAgentAndDate(t *testing.T) {
	when := time.Date(2026, 3, 5, 14, 30, 0, 0, time.UTC)
	item := historyPickerItem{chatID: "c1", title: "A title", agentName: "reviewer", updatedAt: when}
	desc := item.Description()
	if !strings.Contains(desc, "reviewer") {
		t.Errorf("Description() = %q, want it to contain the agent name", desc)
	}
	if !strings.Contains(desc, when.Local().Format(historyDateFormat)) {
		t.Errorf("Description() = %q, want it to contain the formatted date", desc)
	}
}

// ---------------------------------------------------------------------
// removeHistoryItem / confirmDelete / cancelPendingDelete
// (post-v0.1.0 addendum: "d" to delete in ctrl+h's history browser)
// ---------------------------------------------------------------------

func newHistoryTestList(chatIDs ...string) list.Model {
	items := make([]list.Item, 0, len(chatIDs))
	for _, id := range chatIDs {
		items = append(items, historyPickerItem{chatID: id, agentName: "assistant"})
	}
	l := list.New(items, list.NewDefaultDelegate(), 80, 24)
	l.Title = historyListTitle
	return l
}

func TestRemoveHistoryItem_RemovesMatchingChatID(t *testing.T) {
	l := newHistoryTestList("a", "b", "c")
	removeHistoryItem(&l, "b")
	if got := len(l.Items()); got != 2 {
		t.Fatalf("len(Items()) = %d, want 2", got)
	}
	for _, it := range l.Items() {
		if it.(historyPickerItem).chatID == "b" {
			t.Fatal("chat-id \"b\" must be gone after removeHistoryItem")
		}
	}
}

func TestRemoveHistoryItem_UnknownChatID_IsNoop(t *testing.T) {
	l := newHistoryTestList("a", "b")
	removeHistoryItem(&l, "ghost")
	if got := len(l.Items()); got != 2 {
		t.Fatalf("len(Items()) = %d, want 2 (no-op for an id that isn't there)", got)
	}
}

// TestConfirmDelete_RequiresTwoPressesOnSameItem is the direct coverage
// for the "press d again to confirm" flow both ctrl+h call sites share:
// the first call on a given chatID only arms it (returns false, and
// switches the list's header to the confirm prompt); only a second call
// on that *same* chatID, with nothing else in between, actually confirms
// (returns true, and restores the normal header).
func TestConfirmDelete_RequiresTwoPressesOnSameItem(t *testing.T) {
	l := newHistoryTestList("a", "b")
	var pending string

	if confirmDelete(&l, &pending, "a") {
		t.Fatal("the first press must only arm the prompt, not confirm it")
	}
	if pending != "a" {
		t.Errorf("pending = %q, want %q after arming", pending, "a")
	}
	if l.Title != historyDeleteConfirmTitle {
		t.Errorf("Title = %q, want the confirm prompt", l.Title)
	}

	if !confirmDelete(&l, &pending, "a") {
		t.Fatal("a second press on the same chatID must confirm")
	}
	if pending != "" {
		t.Errorf("pending = %q, want empty after confirming", pending)
	}
	if l.Title != historyListTitle {
		t.Errorf("Title = %q, want the normal header restored", l.Title)
	}
}

// TestConfirmDelete_DifferentItem_RestartsConfirmation covers the
// deliberate safety property: pressing "d" on a different item than the
// one currently armed must start a fresh confirmation for the new item,
// never instantly delete it just because something else was armed.
func TestConfirmDelete_DifferentItem_RestartsConfirmation(t *testing.T) {
	l := newHistoryTestList("a", "b")
	var pending string

	confirmDelete(&l, &pending, "a") // arm "a"
	if confirmDelete(&l, &pending, "b") {
		t.Fatal("pressing d on a different item must never confirm on the first press")
	}
	if pending != "b" {
		t.Errorf("pending = %q, want %q (switched to the new item)", pending, "b")
	}
}

func TestCancelPendingDelete_ClearsArmedPromptAndRestoresTitle(t *testing.T) {
	l := newHistoryTestList("a")
	pending := "a"
	l.Title = historyDeleteConfirmTitle

	cancelPendingDelete(&l, &pending)
	if pending != "" {
		t.Errorf("pending = %q, want empty", pending)
	}
	if l.Title != historyListTitle {
		t.Errorf("Title = %q, want the normal header restored", l.Title)
	}
}

func TestCancelPendingDelete_NothingArmed_IsNoop(t *testing.T) {
	l := newHistoryTestList("a")
	l.Title = "unrelated custom title"
	pending := ""

	cancelPendingDelete(&l, &pending)
	if l.Title != "unrelated custom title" {
		t.Errorf("Title = %q, want unchanged when nothing was armed", l.Title)
	}
}
