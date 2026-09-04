package cloudweb

import (
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestSerializeCardPreservesQuestionChoiceContent(t *testing.T) {
	card := core.NewCard().
		Choice("**1. Single**\nDescription", "Single", "askq:0:1", nil).
		MultiSelect([]core.CardMultiSelectOption{
			{Text: "**1. Apple**\nKeeps well", Value: "1"},
			{Text: "**2. Banana**\nBreakfast", Value: "2"},
		}, "Confirm", "askq:1:multi", nil).
		Build()

	elements, ok := serializeCard(card)["elements"].([]map[string]any)
	if !ok || len(elements) != 3 {
		t.Fatalf("elements = %#v, want one choice and two visible multi-select options", serializeCard(card)["elements"])
	}
	if elements[0]["type"] != "list_item" || elements[0]["btn_value"] != "askq:0:1" {
		t.Fatalf("single choice = %#v", elements[0])
	}
	for i := 1; i < 3; i++ {
		if elements[i]["type"] != "markdown" {
			t.Fatalf("multi-select option %d = %#v", i, elements[i])
		}
	}
}
