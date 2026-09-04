package feishu

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func decodeRenderedCard(t *testing.T, card *core.Card) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal([]byte(renderCard(card, "")), &got); err != nil {
		t.Fatalf("renderCard JSON decode failed: %v", err)
	}
	return got
}

func TestRenderCardMap_EqualColumnsActionsUseColumnSet(t *testing.T) {
	buttons := []core.CardButton{
		core.PrimaryBtn("Session Management", "nav:/help session"),
		core.DefaultBtn("Agent Configuration", "nav:/help agent"),
		core.DefaultBtn("Tools & Automation", "nav:/help tools"),
		core.DefaultBtn("System", "nav:/help system"),
	}
	card := core.NewCard().ButtonsEqual(buttons...).Build()
	got := decodeRenderedCard(t, card)

	elements, ok := got["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("elements = %#v, want one element", got["elements"])
	}
	columnSet, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("first element = %#v, want object", elements[0])
	}
	if tag := columnSet["tag"]; tag != "column_set" {
		t.Fatalf("tag = %#v, want column_set", tag)
	}
	columns, ok := columnSet["columns"].([]any)
	if !ok || len(columns) != len(buttons) {
		t.Fatalf("columns = %#v, want %d columns", columnSet["columns"], len(buttons))
	}

	for i, want := range buttons {
		col, ok := columns[i].(map[string]any)
		if !ok {
			t.Fatalf("column %d = %#v, want object", i, columns[i])
		}
		if width := col["width"]; width != "weighted" {
			t.Fatalf("column %d width = %#v, want weighted", i, width)
		}
		if weight := col["weight"]; weight != float64(1) {
			t.Fatalf("column %d weight = %#v, want 1", i, weight)
		}
		innerElems, ok := col["elements"].([]any)
		if !ok || len(innerElems) != 1 {
			t.Fatalf("column %d elements = %#v, want one button", i, col["elements"])
		}
		btn, ok := innerElems[0].(map[string]any)
		if !ok {
			t.Fatalf("column %d button = %#v, want object", i, innerElems[0])
		}
		if tag := btn["tag"]; tag != "button" {
			t.Fatalf("column %d tag = %#v, want button", i, tag)
		}
		text, ok := btn["text"].(map[string]any)
		if !ok || text["content"] != want.Text {
			t.Fatalf("column %d text = %#v, want %q", i, btn["text"], want.Text)
		}
		if btnType := btn["type"]; btnType != want.Type {
			t.Fatalf("column %d type = %#v, want %q", i, btnType, want.Type)
		}
		value, ok := btn["value"].(map[string]any)
		if !ok || value["action"] != want.Value {
			t.Fatalf("column %d value = %#v, want %q", i, btn["value"], want.Value)
		}
	}
}

func TestRenderCardMap_TwoEqualColumnsUseBisectAndCenteredButtons(t *testing.T) {
	buttons := []core.CardButton{
		core.PrimaryBtn("Session Management", "nav:/help session"),
		core.DefaultBtn("Agent Configuration", "nav:/help agent"),
	}
	card := core.NewCard().ButtonsEqual(buttons...).Build()
	got := decodeRenderedCard(t, card)

	elements, ok := got["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("elements = %#v, want one element", got["elements"])
	}
	columnSet, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("first element = %#v, want object", elements[0])
	}
	if flexMode := columnSet["flex_mode"]; flexMode != "bisect" {
		t.Fatalf("flex_mode = %#v, want bisect", flexMode)
	}
	columns, ok := columnSet["columns"].([]any)
	if !ok || len(columns) != len(buttons) {
		t.Fatalf("columns = %#v, want %d columns", columnSet["columns"], len(buttons))
	}
	for i := range buttons {
		col, ok := columns[i].(map[string]any)
		if !ok {
			t.Fatalf("column %d = %#v, want object", i, columns[i])
		}
		if align := col["horizontal_align"]; align != "center" {
			t.Fatalf("column %d horizontal_align = %#v, want center", i, align)
		}
		innerElems, ok := col["elements"].([]any)
		if !ok || len(innerElems) != 1 {
			t.Fatalf("column %d elements = %#v, want one button", i, col["elements"])
		}
		btn, ok := innerElems[0].(map[string]any)
		if !ok {
			t.Fatalf("column %d button = %#v, want object", i, innerElems[0])
		}
		if width := btn["width"]; width != "fill" {
			t.Fatalf("column %d button width = %#v, want fill", i, width)
		}
	}
}

func TestRenderCardMap_DefaultActionsStayActionRow(t *testing.T) {
	buttons := []core.CardButton{
		core.PrimaryBtn("Yes", "act:/yes"),
		core.DefaultBtn("No", "act:/no"),
	}
	card := core.NewCard().Buttons(buttons...).Build()
	got := decodeRenderedCard(t, card)

	elements, ok := got["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("elements = %#v, want one element", got["elements"])
	}
	actionRow, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("first element = %#v, want object", elements[0])
	}
	if tag := actionRow["tag"]; tag != "action" {
		t.Fatalf("tag = %#v, want action", tag)
	}
	actions, ok := actionRow["actions"].([]any)
	if !ok || len(actions) != len(buttons) {
		t.Fatalf("actions = %#v, want %d buttons", actionRow["actions"], len(buttons))
	}
	for i, want := range buttons {
		btn, ok := actions[i].(map[string]any)
		if !ok {
			t.Fatalf("button %d = %#v, want object", i, actions[i])
		}
		if tag := btn["tag"]; tag != "button" {
			t.Fatalf("button %d tag = %#v, want button", i, tag)
		}
		text, ok := btn["text"].(map[string]any)
		if !ok || text["content"] != want.Text {
			t.Fatalf("button %d text = %#v, want %q", i, btn["text"], want.Text)
		}
		if btnType := btn["type"]; btnType != want.Type {
			t.Fatalf("button %d type = %#v, want %q", i, btnType, want.Type)
		}
		value, ok := btn["value"].(map[string]any)
		if !ok || value["action"] != want.Value {
			t.Fatalf("button %d value = %#v, want %q", i, btn["value"], want.Value)
		}
	}
}

func TestRenderCardMap_ChoiceUsesClickableCard2Container(t *testing.T) {
	longLabel := strings.Repeat("Long mobile option ", 8)
	choiceText := "**1. " + longLabel + "**\n" + strings.Repeat("Full explanation ", 10)
	card := core.NewCard().
		Title("Agent Question", "blue").
		Markdown("**Choose an implementation**").
		Choice(choiceText, longLabel, "askq:0:1", map[string]string{
			"askq_label": longLabel, "askq_question": "Choose an implementation",
		}).
		Note("Tap an option to select it.").
		Build()

	rendered, err := json.Marshal(renderCardMap(card, "thread-key"))
	if err != nil {
		t.Fatalf("marshal choice card: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rendered, &got); err != nil {
		t.Fatalf("decode choice card: %v", err)
	}
	if got["schema"] != "2.0" {
		t.Fatalf("schema = %#v, want Card 2.0", got["schema"])
	}
	if _, exists := got["elements"]; exists {
		t.Fatal("Card 2.0 choice card must put elements under body")
	}
	body, ok := got["body"].(map[string]any)
	if !ok {
		t.Fatalf("body = %#v, want object", got["body"])
	}
	elements, ok := body["elements"].([]any)
	if !ok || len(elements) != 3 {
		t.Fatalf("elements = %#v, want question, choice, and note", body["elements"])
	}
	container, ok := elements[1].(map[string]any)
	if !ok || container["tag"] != "interactive_container" {
		t.Fatalf("choice = %#v, want interactive_container", elements[1])
	}
	if container["width"] != "fill" || container["has_border"] != true {
		t.Fatalf("choice container layout = %#v, want full-width bordered block", container)
	}
	inner, ok := container["elements"].([]any)
	if !ok || len(inner) != 1 {
		t.Fatalf("choice elements = %#v, want one markdown element", container["elements"])
	}
	markdown, ok := inner[0].(map[string]any)
	if !ok || markdown["tag"] != "markdown" || markdown["content"] != choiceText {
		t.Fatalf("choice markdown = %#v, want complete content", inner[0])
	}
	behaviors, ok := container["behaviors"].([]any)
	if !ok || len(behaviors) != 1 {
		t.Fatalf("behaviors = %#v, want one callback", container["behaviors"])
	}
	behavior, ok := behaviors[0].(map[string]any)
	if !ok || behavior["type"] != "callback" {
		t.Fatalf("behavior = %#v, want callback", behaviors[0])
	}
	value, ok := behavior["value"].(map[string]any)
	if !ok || value["action"] != "askq:0:1" || value["session_key"] != "thread-key" || value["askq_label"] != longLabel {
		t.Fatalf("callback value = %#v, want preserved action/session/label", behavior["value"])
	}
	if strings.Contains(string(rendered), `"tag":"button"`) {
		t.Fatalf("choice card unexpectedly contains a separate button: %s", rendered)
	}
}

func TestRenderCardMap_MultiSelectUsesCheckerFormAndOneSubmit(t *testing.T) {
	card := core.NewCard().
		Title("Agent Question", "blue").
		Markdown("**Choose fruits**").
		MultiSelect([]core.CardMultiSelectOption{
			{Text: "**1. Apple**\nKeeps well", Value: "1"},
			{Text: "**2. Banana**\nGood for breakfast", Value: "2"},
		}, "Confirm selection", "askq:0:multi", map[string]string{
			choiceCardIDKey: "test-card", "askq_question": "Choose fruits",
		}).
		Note("Select all that apply, then confirm.").
		Build()

	got := renderCardMap(card, "thread-key")
	if got["schema"] != "2.0" {
		t.Fatalf("schema = %#v, want Card 2.0", got["schema"])
	}
	body, ok := got["body"].(map[string]any)
	if !ok {
		t.Fatalf("body = %#v", got["body"])
	}
	elements, ok := body["elements"].([]map[string]any)
	if !ok || len(elements) != 3 {
		t.Fatalf("elements = %#v, want question, form, note", body["elements"])
	}
	form := elements[1]
	if form["tag"] != "form" || form["name"] != "askq_multi_form" {
		t.Fatalf("form = %#v", form)
	}
	formElements, ok := form["elements"].([]map[string]any)
	if !ok || len(formElements) != 3 {
		t.Fatalf("form elements = %#v, want 2 checkers and submit", form["elements"])
	}
	for i := 0; i < 2; i++ {
		checker := formElements[i]
		if checker["tag"] != "checker" || checker["name"] != fmt.Sprintf("askq_option_%d", i+1) {
			t.Fatalf("checker %d = %#v", i, checker)
		}
		if _, exists := checker["behaviors"]; exists {
			t.Fatalf("checker %d triggers a callback instead of toggling locally: %#v", i, checker)
		}
		text, _ := checker["text"].(map[string]any)
		if text["tag"] != "lark_md" || !strings.Contains(fmt.Sprint(text["content"]), "**") {
			t.Fatalf("checker text = %#v, want wrapping markdown", checker["text"])
		}
	}
	submit := formElements[2]
	if submit["tag"] != "button" || submit["form_action_type"] != "submit" || submit["width"] != "fill" || submit["type"] != "primary_filled" {
		t.Fatalf("submit = %#v", submit)
	}
	action, cardID, ok := parseAskQuestionSubmitName(fmt.Sprint(submit["name"]))
	if !ok || action != "askq:0:multi" || cardID != "test-card" {
		t.Fatalf("submit name = %#v, want encoded action and card ID", submit["name"])
	}
	if _, exists := submit["value"]; exists {
		t.Fatalf("Card 2.0 form submit uses deprecated top-level value: %#v", submit)
	}
	if _, exists := submit["behaviors"]; exists {
		t.Fatalf("form submit should not use behaviors: %#v", submit)
	}
}

func TestSelectChoiceCard_UnknownTrackedNonceFailsClosed(t *testing.T) {
	p := &Platform{choiceCards: make(map[string]*choiceCardState)}
	if snapshot, action, accepted := p.selectChoiceCard("missing-card", "askq:0:1"); accepted || snapshot != nil || action != "askq:0:1" {
		t.Fatalf("unknown tracked callback = snapshot %#v action %q accepted %v", snapshot, action, accepted)
	}
	if _, _, accepted := p.selectChoiceCard("", "askq:0:1"); !accepted {
		t.Fatal("legacy callback without a nonce should remain supported")
	}
}

func TestRenderCardMap_DeleteModeUsesCheckerForm(t *testing.T) {
	card := core.NewCard().
		Title("删除会话", "carmine").
		ListItemBtn("☑ **1.** One · **10** msgs · 03-13 20:00", "已选择", "primary", "act:/delete-mode toggle session-1").
		ListItemBtn("▶ **2.** Active · **30** msgs · 03-13 20:01", "当前会话", "primary", "act:/delete-mode noop session-2").
		ListItemBtn("◻ **3.** Three · **20** msgs · 03-13 20:02", "选择", "default", "act:/delete-mode toggle session-3").
		Note("2 selected").
		Buttons(
			core.DangerBtn("删除已选", "act:/delete-mode confirm"),
			core.DefaultBtn("取消", "act:/delete-mode cancel"),
		).
		Buttons(core.DefaultBtn("下一页 →", "act:/delete-mode page 2")).
		Build()

	got := decodeRenderedCard(t, card)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal rendered card failed: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"tag":"form"`) || !strings.Contains(s, `"tag":"checker"`) {
		t.Fatalf("expected form+checker rendering, got %s", s)
	}
	if got := strings.Count(s, `"tag":"checker"`); got != 2 {
		t.Fatalf("checker count = %d, want 2, got %s", got, s)
	}
	if !strings.Contains(s, deleteModeCheckerName("session-1")) {
		t.Fatalf("selectable session checker missing, got %s", s)
	}
	if strings.Contains(s, deleteModeCheckerName("session-2")) {
		t.Fatalf("active session should not render checker, got %s", s)
	}
	if !strings.Contains(s, deleteModeCheckerName("session-3")) {
		t.Fatalf("second selectable session checker missing, got %s", s)
	}
	activeIdx := strings.Index(s, `▶ **2.** Active`)
	firstIdx := strings.Index(s, deleteModeCheckerName("session-1"))
	thirdIdx := strings.Index(s, deleteModeCheckerName("session-3"))
	if activeIdx < 0 || firstIdx < 0 || thirdIdx < 0 {
		t.Fatalf("missing expected order markers in rendered card: %s", s)
	}
	if !(firstIdx < activeIdx && activeIdx < thirdIdx) {
		t.Fatalf("row order changed unexpectedly, got %s", s)
	}
	if !strings.Contains(s, `"name":"delete_mode_form"`) {
		t.Fatalf("expected form name for feishu validation, got %s", s)
	}
	if !strings.Contains(s, `"name":"delete_mode_submit"`) || !strings.Contains(s, `"name":"delete_mode_cancel"`) {
		t.Fatalf("expected button names inside form, got %s", s)
	}
	if !strings.Contains(s, `"form_action_type":"submit"`) || !strings.Contains(s, `act:/delete-mode form-submit`) {
		t.Fatalf("expected form submit action, got %s", s)
	}
	if strings.Contains(s, `act:/delete-mode toggle`) {
		t.Fatalf("expected no toggle buttons in rendered card, got %s", s)
	}
}

func TestRenderCardMap_InjectsSessionKeyIntoCallbacks(t *testing.T) {
	card := core.NewCard().
		Buttons(core.PrimaryBtn("Open", "nav:/help session")).
		ListItem("Choose", "Confirm", "act:/confirm").
		Select("Pick one", []core.CardSelectOption{{Text: "A", Value: "askq:0:1"}}, "").
		Build()

	got := renderCardMap(card, "feishu:oc_chat:root:om_root")
	elements, ok := got["elements"].([]map[string]any)
	if !ok || len(elements) != 3 {
		t.Fatalf("elements = %#v, want 3 elements", got["elements"])
	}

	actionRow := elements[0]
	actions := actionRow["actions"].([]map[string]any)
	firstButton := actions[0]
	value := firstButton["value"].(map[string]string)
	if value["session_key"] != "feishu:oc_chat:root:om_root" {
		t.Fatalf("button session_key = %#v, want thread session key", value["session_key"])
	}

	listRow := elements[1]
	columns := listRow["columns"].([]map[string]any)
	actionCol := columns[1]
	listBtn := actionCol["elements"].([]map[string]any)[0]
	listValue := listBtn["value"].(map[string]string)
	if listValue["session_key"] != "feishu:oc_chat:root:om_root" {
		t.Fatalf("list item session_key = %#v, want thread session key", listValue["session_key"])
	}

	selectRow := elements[2]
	selectActions := selectRow["actions"].([]map[string]any)
	selectValue := selectActions[0]["value"].(map[string]string)
	if selectValue["session_key"] != "feishu:oc_chat:root:om_root" {
		t.Fatalf("select session_key = %#v, want thread session key", selectValue["session_key"])
	}
}

func TestBuildCardJSONWithStatusFooter(t *testing.T) {
	body := "Hello world"
	footer := "Opus 4.7 · ↑ 1 ↓ 168 · 4%\n~/path/to/ws"
	jsonStr := buildCardJSONWithStatusFooter(body, footer)

	var card map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &card); err != nil {
		t.Fatalf("decode card json: %v", err)
	}
	body0 := card["body"].(map[string]any)
	elements := body0["elements"].([]any)
	if len(elements) != 3 {
		t.Fatalf("expected 3 elements (body markdown, hr, footer markdown), got %d: %#v", len(elements), elements)
	}
	bodyEl := elements[0].(map[string]any)
	if bodyEl["tag"] != "markdown" || bodyEl["content"] != body {
		t.Errorf("body element = %#v, want markdown with content %q", bodyEl, body)
	}
	hrEl := elements[1].(map[string]any)
	if hrEl["tag"] != "hr" {
		t.Errorf("middle element = %#v, want hr", hrEl)
	}
	footerEl := elements[2].(map[string]any)
	if footerEl["tag"] != "markdown" {
		t.Errorf("footer tag = %v, want markdown", footerEl["tag"])
	}
	if footerEl["text_size"] != "notation" {
		t.Errorf("footer text_size = %v, want \"notation\"", footerEl["text_size"])
	}
	if footerEl["content"] != footer {
		t.Errorf("footer content = %q, want %q", footerEl["content"], footer)
	}
}

func TestBuildCardJSONWithStatusFooter_EmptyFooterFallsThrough(t *testing.T) {
	body := "Hello"
	a := buildCardJSONWithStatusFooter(body, "")
	b := buildCardJSON(body)
	if a != b {
		t.Errorf("empty footer should match buildCardJSON output\n got: %s\nwant: %s", a, b)
	}
	// whitespace-only footer also falls through
	if got := buildCardJSONWithStatusFooter(body, "   \n  "); got != b {
		t.Errorf("whitespace footer should fall through to buildCardJSON")
	}
}
