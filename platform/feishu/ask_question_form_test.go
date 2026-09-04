package feishu

import (
	"reflect"
	"testing"
)

func TestAskQuestionSubmitNameRoundTrip(t *testing.T) {
	name := askQuestionSubmitName("askq:2:multi", "card-id")
	action, cardID, ok := parseAskQuestionSubmitName(name)
	if !ok || action != "askq:2:multi" || cardID != "card-id" {
		t.Fatalf("parseAskQuestionSubmitName(%q) = %q, %q, %v", name, action, cardID, ok)
	}
}

func TestCollectAskQuestionValuesFromForm(t *testing.T) {
	got := collectAskQuestionValuesFromForm(map[string]any{
		askQuestionCheckerName("3"): true,
		askQuestionCheckerName("1"): "true",
		askQuestionCheckerName("2"): false,
		askQuestionCheckerName("0"): true,
		"unrelated":                 true,
	})
	want := []string{"1", "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected values = %#v, want %#v", got, want)
	}
}
