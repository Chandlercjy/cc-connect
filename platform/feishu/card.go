package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func plainText(content string) map[string]any {
	return map[string]any{"tag": "plain_text", "content": content}
}

// ReplyCard sends a structured card as a reply to the original message.
func (p *interactivePlatform) ReplyCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}

	prepared, cardID := p.prepareChoiceCardForSession(card, rc.sessionKey)
	cardJSON := renderCard(prepared, rc.sessionKey)
	var err error
	if !p.shouldUseThreadOrReplyAPI(rc) {
		if rc.chatID == "" {
			p.forgetChoiceCard(cardID)
			return fmt.Errorf("%s: chatID is empty, cannot send card", p.tag())
		}
		err = p.createMessage(ctx, rc.chatID, larkim.MsgTypeInteractive, cardJSON, "send card")
	} else {
		err = p.replyMessage(ctx, rc, larkim.MsgTypeInteractive, cardJSON)
	}
	if err != nil {
		p.forgetChoiceCard(cardID)
	}
	return err
}

// SendCard sends a structured card as a new message to the chat.
func (p *interactivePlatform) SendCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}
	if rc.chatID == "" {
		return fmt.Errorf("%s: chatID is empty, cannot send card", p.tag())
	}

	if !p.noReplyToTrigger && p.shouldReplyInThread(rc) {
		return p.ReplyCard(ctx, rctx, card)
	}

	prepared, cardID := p.prepareChoiceCardForSession(card, rc.sessionKey)
	cardJSON := renderCard(prepared, rc.sessionKey)
	err := p.createMessage(ctx, rc.chatID, larkim.MsgTypeInteractive, cardJSON, "send card")
	if err != nil {
		p.forgetChoiceCard(cardID)
	}
	return err
}

// RefreshCard updates a previously rendered card in-place using the Patch API.
// It looks up the messageID stored from the most recent card action callback
// for the given session key and patches that message with the new card content.
func (p *interactivePlatform) RefreshCard(ctx context.Context, sessionKey string, card *core.Card) error {
	p.cardActionMsgMu.Lock()
	msgID := p.cardActionMsgIDs[sessionKey]
	p.cardActionMsgMu.Unlock()

	if msgID == "" {
		return fmt.Errorf("%s: no tracked card messageID for session %q", p.tag(), sessionKey)
	}

	cardJSON := renderCard(card, sessionKey)
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(msgID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build()
	return p.withTransientRetry(ctx, "refresh card", func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, "refresh card", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Patch(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: refresh card: %w", p.tag(), err)
			}
			if !resp.Success() {
				return fmt.Errorf("%s: refresh card code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
			}
			return nil
		})
	})
}

const (
	choiceCardIDKey    = "askq_card_id"
	maxChoiceCardState = 256
)

type choiceCardSnapshot struct {
	intro       []string
	choices     []core.CardChoice
	multiSelect *core.CardMultiSelect
}

type choiceCardState struct {
	snapshot       choiceCardSnapshot
	sessionKey     string
	selectedAction string
}

func cardHasChoice(card *core.Card) bool {
	if card == nil {
		return false
	}
	for _, elem := range card.Elements {
		switch elem.(type) {
		case core.CardChoice, core.CardMultiSelect:
			return true
		}
	}
	return false
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return make(map[string]string)
	}
	dst := make(map[string]string, len(src)+1)
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func snapshotChoiceCard(card *core.Card) choiceCardSnapshot {
	var snapshot choiceCardSnapshot
	for _, elem := range card.Elements {
		switch value := elem.(type) {
		case core.CardMarkdown:
			snapshot.intro = append(snapshot.intro, value.Content)
		case core.CardChoice:
			choice := value
			choice.Extra = cloneStringMap(value.Extra)
			snapshot.choices = append(snapshot.choices, choice)
		case core.CardMultiSelect:
			multi := value
			multi.Extra = cloneStringMap(value.Extra)
			multi.Options = append([]core.CardMultiSelectOption(nil), value.Options...)
			snapshot.multiSelect = &multi
		}
	}
	return snapshot
}

// prepareChoiceCard gives one rendered question a unique nonce and remembers
// its complete option list. The nonce is carried by each callback without
// repeating every long option inside the card payload.
func (p *Platform) prepareChoiceCard(card *core.Card) (*core.Card, string) {
	return p.prepareChoiceCardForSession(card, "")
}

func (p *Platform) prepareChoiceCardForSession(card *core.Card, sessionKey string) (*core.Card, string) {
	if !cardHasChoice(card) {
		return card, ""
	}

	prepared := &core.Card{Elements: make([]core.CardElement, 0, len(card.Elements))}
	if card.Header != nil {
		header := *card.Header
		prepared.Header = &header
	}
	cardID := ""
	for _, elem := range card.Elements {
		switch value := elem.(type) {
		case core.CardChoice:
			choice := value
			choice.Extra = cloneStringMap(choice.Extra)
			if cardID == "" {
				cardID = choice.Extra[choiceCardIDKey]
				if cardID == "" {
					cardID = uuid.NewString()
				}
			}
			choice.Extra[choiceCardIDKey] = cardID
			prepared.Elements = append(prepared.Elements, choice)
		case core.CardMultiSelect:
			multi := value
			multi.Extra = cloneStringMap(multi.Extra)
			multi.Options = append([]core.CardMultiSelectOption(nil), multi.Options...)
			if cardID == "" {
				cardID = multi.Extra[choiceCardIDKey]
				if cardID == "" {
					cardID = uuid.NewString()
				}
			}
			multi.Extra[choiceCardIDKey] = cardID
			prepared.Elements = append(prepared.Elements, multi)
		default:
			prepared.Elements = append(prepared.Elements, elem)
		}
	}

	state := &choiceCardState{snapshot: snapshotChoiceCard(prepared), sessionKey: sessionKey}
	p.choiceCardMu.Lock()
	if p.choiceCards == nil {
		p.choiceCards = make(map[string]*choiceCardState)
	}
	if len(p.choiceCards) >= maxChoiceCardState {
		// Keep active prompts tracked so duplicate protection never fails open.
		// Completed cards remain useful only for stale redraws and are the safe
		// entries to evict as new prompts arrive.
		for staleID, staleState := range p.choiceCards {
			if staleState.selectedAction != "" {
				delete(p.choiceCards, staleID)
				break
			}
		}
	}
	p.choiceCards[cardID] = state
	p.choiceCardMu.Unlock()
	return prepared, cardID
}

func (p *Platform) choiceCardSessionKey(cardID string) string {
	if cardID == "" {
		return ""
	}
	p.choiceCardMu.Lock()
	defer p.choiceCardMu.Unlock()
	if state := p.choiceCards[cardID]; state != nil {
		return state.sessionKey
	}
	return ""
}

func (p *Platform) forgetChoiceCard(cardID string) {
	if cardID == "" {
		return
	}
	p.choiceCardMu.Lock()
	delete(p.choiceCards, cardID)
	p.choiceCardMu.Unlock()
}

// selectChoiceCard atomically records the first answer. Later callbacks for
// the same card return the original selection with accepted=false, allowing
// the caller to redraw the immutable result without dispatching twice.
func (p *Platform) selectChoiceCard(cardID, requestedAction string) (*choiceCardSnapshot, string, bool) {
	if cardID == "" {
		return nil, requestedAction, true
	}
	p.choiceCardMu.Lock()
	defer p.choiceCardMu.Unlock()
	state := p.choiceCards[cardID]
	if state == nil {
		// A nonce-bearing callback without state is stale, evicted, forged, or
		// from before a restart. Failing closed is safer than dispatching it as
		// a fresh answer to whichever request happens to be pending now.
		return nil, requestedAction, false
	}
	if state.selectedAction == "" {
		state.selectedAction = requestedAction
		return &state.snapshot, requestedAction, true
	}
	return &state.snapshot, state.selectedAction, false
}

func choiceMetadata(snapshot *choiceCardSnapshot, action string) (question, answer, answeredLabel, requestID string) {
	if snapshot == nil {
		return "", "", "", ""
	}
	if len(snapshot.intro) > 0 {
		question = snapshot.intro[0]
	}
	for _, choice := range snapshot.choices {
		if choice.Value != action {
			continue
		}
		answer = choice.Extra["askq_label"]
		if answer == "" {
			answer = choice.ButtonText
		}
		answeredLabel = choice.Extra["askq_answered"]
		requestID = choice.Extra["askq_request_id"]
		break
	}
	if snapshot.multiSelect != nil {
		if q := snapshot.multiSelect.Extra["askq_question"]; q != "" {
			question = q
		}
		answeredLabel = snapshot.multiSelect.Extra["askq_answered"]
		requestID = snapshot.multiSelect.Extra["askq_request_id"]
		answer = strings.Join(multiSelectValuesFromAction(action), ",")
	}
	return question, answer, answeredLabel, requestID
}

func multiSelectValuesFromAction(action string) []string {
	parts := strings.SplitN(action, ":", 4)
	if len(parts) != 4 || parts[0] != "askq" || parts[2] != "multi" {
		return nil
	}
	var values []string
	for _, value := range strings.Split(parts[3], ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func cardCallbackValue(action, sessionKey string, extra map[string]string) map[string]string {
	value := map[string]string{"action": action}
	if sessionKey != "" {
		value["session_key"] = sessionKey
	}
	for k, v := range extra {
		value[k] = v
	}
	return value
}

func renderCard2Button(button core.CardButton, sessionKey string) map[string]any {
	buttonType := button.Type
	if buttonType == "" {
		buttonType = "default"
	}
	return map[string]any{
		"tag":  "button",
		"text": plainText(button.Text),
		"type": buttonType,
		"behaviors": []map[string]any{{
			"type":  "callback",
			"value": cardCallbackValue(button.Value, sessionKey, button.Extra),
		}},
	}
}

// renderChoiceCardMap converts a card containing CardChoice elements into a
// Feishu Card 2.0 payload. Card 2.0 interactive_container is the only native
// Feishu component that makes a wrapping content block itself clickable.
func renderChoiceCardMap(card *core.Card, sessionKey string) map[string]any {
	result := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   "default",
		},
	}
	if card.Header != nil && card.Header.Title != "" {
		color := card.Header.Color
		if color == "" {
			color = "blue"
		}
		result["header"] = map[string]any{
			"title":    plainText(card.Header.Title),
			"template": color,
		}
	}

	var elements []map[string]any
	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardMarkdown:
			elements = append(elements, map[string]any{
				"tag": "markdown", "content": e.Content,
			})
		case core.CardDivider:
			elements = append(elements, map[string]any{"tag": "hr"})
		case core.CardChoice:
			elements = append(elements, map[string]any{
				"tag":           "interactive_container",
				"width":         "fill",
				"has_border":    true,
				"border_color":  "grey",
				"corner_radius": "8px",
				"padding":       "8px 12px 8px 12px",
				"behaviors": []map[string]any{{
					"type":  "callback",
					"value": cardCallbackValue(e.Value, sessionKey, e.Extra),
				}},
				"elements": []map[string]any{{
					"tag": "markdown", "content": e.Text,
				}},
			})
		case core.CardMultiSelect:
			formElements := make([]map[string]any, 0, len(e.Options)+1)
			for _, option := range e.Options {
				formElements = append(formElements, map[string]any{
					"tag":               "checker",
					"name":              askQuestionCheckerName(option.Value),
					"checked":           false,
					"overall_checkable": true,
					"text": map[string]any{
						"tag": "lark_md", "content": option.Text,
					},
				})
			}
			formElements = append(formElements, map[string]any{
				"tag":              "button",
				"name":             askQuestionSubmitName(e.Value, e.Extra[choiceCardIDKey]),
				"text":             plainText(e.SubmitText),
				"type":             "primary_filled",
				"width":            "fill",
				"form_action_type": "submit",
				// Card 2.0 form buttons deliberately use name + form_action_type
				// rather than legacy top-level value or callback behaviors.
			})
			elements = append(elements, map[string]any{
				"tag":              "form",
				"name":             "askq_multi_form",
				"direction":        "vertical",
				"vertical_spacing": "medium",
				"elements":         formElements,
			})
		case core.CardActions:
			buttons := make([]map[string]any, 0, len(e.Buttons))
			for _, button := range e.Buttons {
				buttons = append(buttons, renderCard2Button(button, sessionKey))
			}
			if len(buttons) == 1 {
				buttons[0]["width"] = "fill"
				elements = append(elements, buttons[0])
			} else if len(buttons) > 1 {
				columns := make([]map[string]any, 0, len(buttons))
				for _, button := range buttons {
					button["width"] = "fill"
					columns = append(columns, map[string]any{
						"tag": "column", "width": "weighted", "weight": 1,
						"elements": []map[string]any{button},
					})
				}
				columnSet := map[string]any{"tag": "column_set", "columns": columns}
				if len(buttons) == 2 {
					columnSet["flex_mode"] = "bisect"
				}
				elements = append(elements, columnSet)
			}
		case core.CardListItem:
			button := renderCard2Button(core.CardButton{
				Text: e.BtnText, Type: e.BtnType, Value: e.BtnValue, Extra: e.Extra,
			}, sessionKey)
			elements = append(elements, map[string]any{
				"tag": "column_set", "flex_mode": "none",
				"columns": []map[string]any{
					{"tag": "column", "width": "weighted", "weight": 5,
						"elements": []map[string]any{{"tag": "markdown", "content": e.Text}}},
					{"tag": "column", "width": "auto", "vertical_align": "center",
						"elements": []map[string]any{button}},
				},
			})
		case core.CardSelect:
			options := make([]map[string]any, 0, len(e.Options))
			for _, option := range e.Options {
				options = append(options, map[string]any{
					"text": plainText(option.Text), "value": option.Value,
				})
			}
			selectElement := map[string]any{
				"tag": "select_static", "placeholder": plainText(e.Placeholder),
				"options": options, "width": "fill",
			}
			if sessionKey != "" {
				selectElement["behaviors"] = []map[string]any{{
					"type": "callback", "value": map[string]string{"session_key": sessionKey},
				}}
			}
			if e.InitValue != "" {
				selectElement["initial_option"] = e.InitValue
			}
			elements = append(elements, selectElement)
		case core.CardNote:
			elements = append(elements, map[string]any{
				"tag": "markdown", "content": e.Text, "text_size": "notation",
			})
		}
	}
	result["body"] = map[string]any{
		"direction":        "vertical",
		"vertical_spacing": "medium",
		"elements":         elements,
	}
	return result
}

// renderAnsweredChoiceCardMap creates the immutable replacement returned after
// a Card 2.0 choice is selected. It retains the original question and every
// option for chat-history context, marks the selected option, and deliberately
// omits all behaviors so the card cannot be submitted again.
func renderAnsweredChoiceCardMap(snapshot *choiceCardSnapshot, selectedAction, question, answer, answeredLabel string) map[string]any {
	if answeredLabel == "" {
		answeredLabel = "Answered"
	}
	var elements []map[string]any
	if snapshot != nil {
		for _, intro := range snapshot.intro {
			if intro != "" {
				elements = append(elements, map[string]any{
					"tag": "markdown", "content": intro,
				})
			}
		}
		if snapshot.multiSelect != nil {
			selected := make(map[string]bool)
			for _, value := range multiSelectValuesFromAction(selectedAction) {
				selected[value] = true
			}
			for index, option := range snapshot.multiSelect.Options {
				prefix := "☐ "
				if selected[option.Value] {
					prefix = "✅ "
				}
				elements = append(elements, map[string]any{
					"tag": "markdown", "content": prefix + option.Text,
				})
				if index+1 < len(snapshot.multiSelect.Options) {
					elements = append(elements, map[string]any{"tag": "hr"})
				}
			}
		} else {
			for index, choice := range snapshot.choices {
				content := choice.Text
				if choice.Value == selectedAction {
					content = "✅ " + content
				}
				elements = append(elements, map[string]any{
					"tag": "markdown", "content": content,
				})
				if index+1 < len(snapshot.choices) {
					elements = append(elements, map[string]any{"tag": "hr"})
				}
			}
		}
	} else {
		if question != "" {
			elements = append(elements, map[string]any{
				"tag": "markdown", "content": question,
			})
		}
		elements = append(elements, map[string]any{
			"tag": "markdown", "content": "✅ **→ " + answer + "**",
		})
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   "default",
		},
		"header": map[string]any{
			"title": plainText("✅ " + answeredLabel), "template": "green",
		},
		"body": map[string]any{
			"direction": "vertical", "elements": elements,
		},
	}
}

// renderCardMap converts a core.Card into a Feishu Interactive Card map. Cards
// with selectable content blocks use Card 2.0; all existing cards retain the
// legacy v1 format. Used by both message sends and callback responses.
func renderCardMap(card *core.Card, sessionKey string) map[string]any {
	if cardHasChoice(card) {
		return renderChoiceCardMap(card, sessionKey)
	}
	result := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
		},
	}
	if card == nil {
		return result
	}

	if card.Header != nil && card.Header.Title != "" {
		color := card.Header.Color
		if color == "" {
			color = "blue"
		}
		result["header"] = map[string]any{
			"title":    plainText(card.Header.Title),
			"template": color,
		}
	}
	if transformed, ok := renderDeleteModeCheckerCard(card, result); ok {
		return transformed
	}

	var elements []map[string]any
	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardMarkdown:
			elements = append(elements, map[string]any{
				"tag":     "markdown",
				"content": e.Content,
			})
		case core.CardDivider:
			elements = append(elements, map[string]any{
				"tag": "hr",
			})
		case core.CardActions:
			var actions []map[string]any
			for _, btn := range e.Buttons {
				btnType := btn.Type
				if btnType == "" {
					btnType = "default"
				}
				valMap := map[string]string{"action": btn.Value}
				if sessionKey != "" {
					valMap["session_key"] = sessionKey
				}
				for k, v := range btn.Extra {
					valMap[k] = v
				}
				action := map[string]any{
					"tag":   "button",
					"text":  plainText(btn.Text),
					"type":  btnType,
					"value": valMap,
				}
				if e.Layout == core.CardActionLayoutEqualColumns {
					action["width"] = "fill"
				}
				actions = append(actions, action)
			}
			if len(actions) > 0 {
				if e.Layout == core.CardActionLayoutEqualColumns {
					columns := make([]map[string]any, 0, len(actions))
					for _, action := range actions {
						columns = append(columns, map[string]any{
							"tag":              "column",
							"width":            "weighted",
							"weight":           1,
							"vertical_align":   "center",
							"horizontal_align": "center",
							"elements":         []map[string]any{action},
						})
					}
					columnSet := map[string]any{
						"tag":     "column_set",
						"columns": columns,
					}
					if len(actions) == 2 {
						columnSet["flex_mode"] = "bisect"
					}
					elements = append(elements, columnSet)
				} else {
					elements = append(elements, map[string]any{
						"tag":     "action",
						"actions": actions,
					})
				}
			}
		case core.CardListItem:
			btnType := e.BtnType
			if btnType == "" {
				btnType = "default"
			}
			valMap := map[string]string{"action": e.BtnValue}
			if sessionKey != "" {
				valMap["session_key"] = sessionKey
			}
			for k, v := range e.Extra {
				valMap[k] = v
			}
			elements = append(elements, map[string]any{
				"tag":       "column_set",
				"flex_mode": "none",
				"columns": []map[string]any{
					{
						"tag":            "column",
						"width":          "weighted",
						"weight":         5,
						"vertical_align": "center",
						"elements": []map[string]any{
							{
								"tag":     "markdown",
								"content": e.Text,
							},
						},
					},
					{
						"tag":            "column",
						"width":          "auto",
						"vertical_align": "center",
						"elements": []map[string]any{
							{
								"tag":   "button",
								"text":  plainText(e.BtnText),
								"type":  btnType,
								"value": valMap,
							},
						},
					},
				},
			})
		case core.CardSelect:
			var options []map[string]any
			for _, opt := range e.Options {
				options = append(options, map[string]any{
					"text":  plainText(opt.Text),
					"value": opt.Value,
				})
			}
			selectElem := map[string]any{
				"tag":         "select_static",
				"placeholder": plainText(e.Placeholder),
				"options":     options,
			}
			if sessionKey != "" {
				selectElem["value"] = map[string]string{"session_key": sessionKey}
			}
			if e.InitValue != "" {
				selectElem["initial_option"] = e.InitValue
			}
			elements = append(elements, map[string]any{
				"tag":     "action",
				"actions": []map[string]any{selectElem},
			})
		case core.CardNote:
			elements = append(elements, map[string]any{
				"tag":      "note",
				"elements": []map[string]any{plainText(e.Text)},
			})
		}
	}

	if len(elements) == 0 {
		elements = []map[string]any{{"tag": "markdown", "content": " "}}
	}

	result["elements"] = elements
	return result
}

type deleteModeCheckerRow struct {
	id      string
	text    string
	checked bool
}

func renderDeleteModeCheckerCard(card *core.Card, base map[string]any) (map[string]any, bool) {
	if card == nil {
		return nil, false
	}

	formRowElements := make([]map[string]any, 0)
	notes := make([]core.CardNote, 0)
	navRows := make([]core.CardActions, 0)
	submitText := ""
	cancelText := ""

	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardListItem:
			id, selectable, ok := parseDeleteModeListItemAction(e.BtnValue)
			if !ok {
				return nil, false
			}
			text := normalizeDeleteModeCheckerText(e.Text)
			if !selectable {
				formRowElements = append(formRowElements, map[string]any{
					"tag":     "markdown",
					"content": "▶ " + text,
				})
				continue
			}
			row := deleteModeCheckerRow{
				id:      id,
				text:    text,
				checked: strings.Contains(e.Text, "☑"),
			}
			formRowElements = append(formRowElements, map[string]any{
				"tag":     "checker",
				"name":    deleteModeCheckerName(row.id),
				"checked": row.checked,
				"text": map[string]any{
					"tag":     "lark_md",
					"content": row.text,
				},
			})
		case core.CardNote:
			notes = append(notes, e)
		case core.CardActions:
			remaining := make([]core.CardButton, 0, len(e.Buttons))
			for _, btn := range e.Buttons {
				switch btn.Value {
				case "act:/delete-mode confirm":
					submitText = btn.Text
				case "act:/delete-mode cancel":
					cancelText = btn.Text
				default:
					remaining = append(remaining, btn)
				}
			}
			if len(remaining) > 0 {
				navRows = append(navRows, core.CardActions{Buttons: remaining, Layout: e.Layout})
			}
		case core.CardMarkdown, core.CardDivider, core.CardSelect:
			return nil, false
		}
	}

	if len(formRowElements) == 0 || submitText == "" {
		return nil, false
	}

	elements := make([]map[string]any, 0, len(notes)+1+len(navRows))
	for _, n := range notes {
		if n.Text == "" {
			continue
		}
		if n.Tag == "delete-mode-selected-count" {
			continue
		}
		elements = append(elements, map[string]any{
			"tag":      "note",
			"elements": []map[string]any{plainText(n.Text)},
		})
	}
	formElements := append([]map[string]any{}, formRowElements...)

	buttonColumns := []map[string]any{
		{
			"tag":            "column",
			"width":          "auto",
			"vertical_align": "center",
			"elements": []map[string]any{
				{
					"tag":              "button",
					"text":             plainText(submitText),
					"type":             "danger",
					"name":             "delete_mode_submit",
					"form_action_type": "submit",
					"value":            map[string]string{"action": "act:/delete-mode form-submit"},
				},
			},
		},
	}
	if cancelText != "" {
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":            "column",
			"width":          "auto",
			"vertical_align": "center",
			"elements": []map[string]any{
				{
					"tag":   "button",
					"text":  plainText(cancelText),
					"type":  "default",
					"name":  "delete_mode_cancel",
					"value": map[string]string{"action": "act:/delete-mode cancel"},
				},
			},
		})
	}
	formElements = append(formElements, map[string]any{
		"tag":              "column_set",
		"horizontal_align": "left",
		"columns":          buttonColumns,
	})

	elements = append(elements, map[string]any{
		"tag":      "form",
		"name":     "delete_mode_form",
		"elements": formElements,
	})

	for _, row := range navRows {
		actions := make([]map[string]any, 0, len(row.Buttons))
		for _, btn := range row.Buttons {
			btnType := btn.Type
			if btnType == "" {
				btnType = "default"
			}
			valMap := map[string]string{"action": btn.Value}
			for k, v := range btn.Extra {
				valMap[k] = v
			}
			action := map[string]any{
				"tag":   "button",
				"text":  plainText(btn.Text),
				"type":  btnType,
				"value": valMap,
			}
			if row.Layout == core.CardActionLayoutEqualColumns {
				action["width"] = "fill"
			}
			actions = append(actions, action)
		}
		if len(actions) > 0 {
			elements = append(elements, map[string]any{
				"tag":     "action",
				"actions": actions,
			})
		}
	}

	base["elements"] = elements
	return base, true
}

func normalizeDeleteModeCheckerText(text string) string {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"☑ ▶", "◻ ▶", "▶", "☑", "◻"} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
			break
		}
	}
	return trimmed
}

func parseDeleteModeListItemAction(action string) (id string, selectable bool, ok bool) {
	const (
		togglePrefix = "act:/delete-mode toggle "
		noopPrefix   = "act:/delete-mode noop "
	)
	switch {
	case strings.HasPrefix(action, togglePrefix):
		id = strings.TrimSpace(strings.TrimPrefix(action, togglePrefix))
		return id, true, id != ""
	case strings.HasPrefix(action, noopPrefix):
		id = strings.TrimSpace(strings.TrimPrefix(action, noopPrefix))
		return id, false, id != ""
	default:
		return "", false, false
	}
}

// renderCard converts a core.Card into the Feishu Interactive Card JSON string.
func renderCard(card *core.Card, sessionKey string) string {
	b, err := json.Marshal(renderCardMap(card, sessionKey))
	if err != nil {
		slog.Error("feishu: renderCard marshal failed", "error", err)
		return `{"config":{"wide_screen_mode":true},"elements":[]}`
	}
	return string(b)
}
