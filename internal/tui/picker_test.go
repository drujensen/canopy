package tui

import (
	"strings"
	"testing"
	"time"
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
