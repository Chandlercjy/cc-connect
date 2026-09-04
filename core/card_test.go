package core

import "testing"

func TestCardRenderText_IncludesAllElementTypes(t *testing.T) {
	card := NewCard().
		Title("Help", "blue").
		Markdown("Use `/help` to see commands.").
		Divider().
		Buttons(PrimaryBtn("Run", "cmd:/run"), DefaultBtn("Cancel", "cmd:/cancel")).
		ListItemBtn("Current session", "Switch", "primary", "act:/switch 1").
		Choice("**1. Complete option**\nDetailed explanation", "Complete option", "askq:0:1", nil).
		Select("Mode", []CardSelectOption{{Text: "Default", Value: "default"}, {Text: "YOLO", Value: "yolo"}}, "default").
		Note("Tip: /new starts a fresh session.").
		Build()

	got := card.RenderText()
	want := "**Help**\n\nUse `/help` to see commands.\n\n---\n\n[Run]  [Cancel]\n\nCurrent session  [Switch]\n**1. Complete option**\nDetailed explanation\n\nMode: Default | YOLO\n\nTip: /new starts a fresh session."
	if got != want {
		t.Fatalf("RenderText() = %q, want %q", got, want)
	}
}

func TestCardHasButtons_DetectsInteractiveElements(t *testing.T) {
	tests := []struct {
		name string
		card *Card
		want bool
	}{
		{
			name: "no interactive elements",
			card: NewCard().Markdown("Plain text only").Build(),
			want: false,
		},
		{
			name: "action row buttons",
			card: NewCard().Buttons(DefaultBtn("Open", "cmd:/open")).Build(),
			want: true,
		},
		{
			name: "list item button",
			card: NewCard().ListItem("Session A", "Switch", "act:/switch 1").Build(),
			want: true,
		},
		{
			name: "clickable choice",
			card: NewCard().Choice("Complete option", "Option", "askq:0:1", nil).Build(),
			want: true,
		},
		{
			name: "select dropdown",
			card: NewCard().Select("Mode", []CardSelectOption{{Text: "Default", Value: "default"}}, "default").Build(),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.card.HasButtons(); got != tt.want {
				t.Fatalf("HasButtons() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCardChoice_CollectButtonsProvidesFallback(t *testing.T) {
	card := NewCard().Choice("**1. Complete option**", "Complete option", "askq:0:1", nil).Build()

	rows := card.CollectButtons()
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("CollectButtons() = %#v, want one fallback button", rows)
	}
	if rows[0][0].Text != "Complete option" || rows[0][0].Data != "askq:0:1" {
		t.Fatalf("fallback button = %#v", rows[0][0])
	}
}
