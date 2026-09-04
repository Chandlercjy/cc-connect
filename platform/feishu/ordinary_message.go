package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	ordinaryMessageModeLegacy = "legacy"
	ordinaryMessageModeHermes = "hermes"

	ordinaryPreviewIntermediatePUTLimit = 19
	ordinaryPreviewTotalPUTLimit        = 20

	// Feishu caps post request bodies at 30 KB. Keep the nested post content
	// below 28 KB as well, then verify the actual serialized SDK request body
	// (whose content field escapes that JSON a second time) against 30 KB.
	ordinaryPostContentPayloadLimit = 28_000
	ordinaryPostRequestBodyLimit    = 30_000
	ordinaryTextChunkRuneLimit      = 4_000

	ordinaryRequestUUIDPlaceholder = "00000000-0000-0000-0000-000000000000"
)

type feishuPreviewKind string

const (
	feishuPreviewKindCard     feishuPreviewKind = "card"
	feishuPreviewKindOrdinary feishuPreviewKind = "ordinary"
)

var (
	errOrdinaryFinalPUTUnavailable = errors.New("ordinary final preview PUT unavailable")
	feishuMentionTagPattern        = regexp.MustCompile(`(?s)<at\s+(?:user_id|id)\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)[^>]*>.*?</at>`)
)

type ordinaryPostImage struct {
	imageKey string
	alt      string
	rawURL   string
}

type ordinaryPostChunk struct {
	markdown string
	images   []ordinaryPostImage
	footer   string
	body     string
}

func (c ordinaryPostChunk) readableFallback() string {
	text := strings.TrimRight(c.markdown, "\n")
	for _, image := range c.images {
		fallback := ordinaryMarkdownImageFallback(image.alt, image.rawURL)
		if text != "" {
			text += "\n\n"
		}
		text += fallback
	}
	return appendOrdinaryStatusFooter(text, c.footer)
}

type ordinaryMessageAPIError struct {
	operation string
	code      int
	msg       string
}

func (e *ordinaryMessageAPIError) Error() string {
	return fmt.Sprintf("%s failed code=%d msg=%s", e.operation, e.code, e.msg)
}

func isInvalidPostContentError(err error) bool {
	var apiErr *ordinaryMessageAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	msg := strings.ToLower(apiErr.msg)
	if strings.Contains(msg, "content format of the post type is incorrect") {
		return true
	}
	if apiErr.code != 230001 {
		return false
	}
	return strings.Contains(msg, "invalid post") ||
		strings.Contains(msg, "invalid href") ||
		(strings.Contains(msg, "post") && strings.Contains(msg, "content") &&
			(strings.Contains(msg, "invalid") || strings.Contains(msg, "incorrect")))
}

func (p *Platform) hermesOrdinaryMessagesEnabled() bool {
	return p.ordinaryMessageMode == ordinaryMessageModeHermes
}

func hermesInteractiveMessageBody(content string) (string, bool) {
	if isHermesCardJSON(content) {
		return content, true
	}
	if payload, ok := core.ParseProgressCardPayload(content); ok {
		return buildProgressCardJSONFromPayload(payload), true
	}
	return "", false
}

func isInteractivePreviewContent(content string) bool {
	_, ok := hermesInteractiveMessageBody(content)
	return ok
}

func extractHermesCardMarkdown(cardJSON string) string {
	var card map[string]any
	if err := json.Unmarshal([]byte(cardJSON), &card); err != nil {
		return ""
	}

	parts := make([]string, 0, 4)
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			if tag, _ := typed["tag"].(string); tag == "markdown" {
				if content, _ := typed["content"].(string); strings.TrimSpace(content) != "" {
					parts = append(parts, strings.TrimSpace(content))
				}
				return
			}

			preferred := []string{"body", "elements", "columns", "items"}
			visited := make(map[string]struct{}, len(preferred))
			for _, key := range preferred {
				if child, exists := typed[key]; exists {
					visited[key] = struct{}{}
					walk(child)
				}
			}
			keys := make([]string, 0, len(typed))
			for key, child := range typed {
				if _, ok := visited[key]; ok {
					continue
				}
				switch child.(type) {
				case []any, map[string]any:
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(typed[key])
			}
		}
	}
	walk(card["body"])
	return strings.Join(parts, "\n\n")
}

func hermesCardOrdinaryFallback(content string) (string, bool) {
	cardJSON, ok := hermesInteractiveMessageBody(content)
	if !ok {
		return content, false
	}
	if markdown := extractHermesCardMarkdown(cardJSON); markdown != "" {
		return markdown, true
	}
	return content, true
}

func isOrdinaryMarkdown(content string) bool {
	if containsMarkdown(content) || hasComplexMarkdown(content) {
		return true
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"# ", "## ", "### ", "> ", "- ", "* ", "+ ", "1. "} {
			if strings.HasPrefix(trimmed, prefix) {
				return true
			}
		}
	}
	return strings.Contains(content, "](")
}

func buildOrdinaryMessageContent(content string, forcePost bool) (msgType, body string) {
	hasMention := strings.Contains(content, `<at user_id=`) || strings.Contains(content, `<at id=`)
	if !forcePost && (!isOrdinaryMarkdown(content) || hasMention) {
		b, _ := json.Marshal(map[string]string{"text": content})
		return larkim.MsgTypeText, string(b)
	}
	return larkim.MsgTypePost, buildPostMdJSON(content)
}

func (p *Platform) sendHermesMessage(ctx context.Context, rc replyContext, content string) error {
	if interactiveBody, ok := hermesInteractiveMessageBody(content); ok {
		if p.useInteractiveCard {
			if p.shouldUseThreadOrReplyAPI(rc) {
				return p.replyMessage(ctx, rc, larkim.MsgTypeInteractive, interactiveBody)
			}
			return p.sendNewMessageToChat(ctx, rc, larkim.MsgTypeInteractive, interactiveBody)
		}
		if markdown := extractHermesCardMarkdown(interactiveBody); markdown != "" {
			content = markdown
		}
	}

	msgType, _ := buildOrdinaryMessageContent(content, false)
	if msgType != larkim.MsgTypePost {
		_, err := p.sendOrdinaryTextChunks(ctx, rc, content)
		return err
	}

	prepared, images := p.prepareOrdinaryPostContent(ctx, content, true)
	chunks, err := p.buildOrdinaryFinalChunks(rc, prepared, "", images)
	if err != nil {
		return fmt.Errorf("%s: build Hermes ordinary post chunks: %w", p.tag(), err)
	}
	_, err = p.sendOrdinaryPostChunks(ctx, rc, chunks)
	return err
}

func appendOrdinaryStatusFooter(content, footer string) string {
	if strings.TrimSpace(footer) == "" {
		return content
	}
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return footer
	}
	return content + "\n\n" + footer
}

func hasFeishuMentionTag(content string) bool {
	return strings.Contains(content, `<at user_id=`) || strings.Contains(content, `<at id=`)
}

func buildMarkdownPostRows(content string) [][]map[string]any {
	if content == "" {
		return [][]map[string]any{{{"tag": "md", "text": ""}}}
	}
	if !strings.Contains(content, "```") {
		return [][]map[string]any{{{"tag": "md", "text": content}}}
	}

	rows := make([][]map[string]any, 0, 3)
	current := make([]string, 0)
	inCodeBlock := false
	flush := func() {
		if len(current) == 0 {
			return
		}
		segment := strings.Join(current, "\n")
		if strings.TrimSpace(segment) != "" {
			rows = append(rows, []map[string]any{{"tag": "md", "text": segment}})
		}
		current = current[:0]
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		isFence := false
		if inCodeBlock {
			isFence = trimmed == "```"
		} else if strings.HasPrefix(trimmed, "```") {
			isFence = !strings.Contains(trimmed[3:], "`")
		}
		if isFence {
			if !inCodeBlock {
				flush()
			}
			current = append(current, line)
			inCodeBlock = !inCodeBlock
			if !inCodeBlock {
				flush()
			}
			continue
		}
		current = append(current, line)
	}
	flush()
	if len(rows) == 0 {
		return [][]map[string]any{{{"tag": "md", "text": content}}}
	}
	return rows
}

func buildOrdinaryPostJSON(content string, images []ordinaryPostImage) string {
	return buildOrdinaryPostJSONWithFooter(content, images, "")
}

func buildOrdinaryPostJSONWithFooter(content string, images []ordinaryPostImage, footer string) string {
	rows := buildMarkdownPostRows(sanitizeMarkdownURLs(content))
	for _, image := range images {
		rows = append(rows, []map[string]any{{
			"tag":       "img",
			"image_key": image.imageKey,
		}})
	}
	if strings.TrimSpace(footer) != "" {
		footerText := footer
		if content != "" {
			footerText = "\n\n" + footerText
		}
		rows = append(rows, []map[string]any{{
			"tag":  "md",
			"text": sanitizeMarkdownURLs(footerText),
		}})
	}
	post := map[string]any{
		"zh_cn": map[string]any{
			"content": rows,
		},
	}
	b, _ := json.Marshal(post)
	return string(b)
}

func ordinaryMarkdownImageFallback(alt, value string) string {
	label := strings.TrimSpace(alt)
	if label == "" {
		label = "image"
	}
	if isRemoteRichCardImageURL(value) {
		return fmt.Sprintf("[%s](%s)", label, value)
	}
	return fmt.Sprintf("%s (%s)", label, value)
}

// prepareOrdinaryPostContent shares the rich-card upload cache and upload
// pipeline. Intermediate frames only start uploads and retain clickable
// placeholders; final frames wait up to the same four-second rich-card budget.
func (p *Platform) prepareOrdinaryPostContent(ctx context.Context, markdown string, final bool) (string, []ordinaryPostImage) {
	if !strings.Contains(markdown, "![") {
		return markdown, nil
	}

	selected := make(map[string]*richCardImageUpload)
	selectedOccurrences := 0
	for _, parts := range feishuCardImagePattern.FindAllStringSubmatch(markdown, -1) {
		if len(parts) != 3 {
			continue
		}
		rawURL := parts[2]
		if !isRemoteRichCardImageURL(rawURL) {
			continue
		}
		selectedOccurrences++
		if selectedOccurrences > richCardImageMaxCount {
			continue
		}
		if _, exists := selected[rawURL]; !exists {
			selected[rawURL] = p.ensureRichCardImageUpload(ctx, rawURL)
		}
	}

	if final {
		pending := make(map[*richCardImageUpload]struct{}, len(selected))
		for _, upload := range selected {
			if upload != nil {
				pending[upload] = struct{}{}
			}
		}
		waitForRichCardImageUploads(ctx, pending)
	}

	images := make([]ordinaryPostImage, 0, richCardImageMaxCount)
	occurrence := 0
	resolvedText := feishuCardImagePattern.ReplaceAllStringFunc(markdown, func(match string) string {
		parts := feishuCardImagePattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		alt, value := parts[1], parts[2]
		if !isRemoteRichCardImageURL(value) {
			return ordinaryMarkdownImageFallback(alt, value)
		}
		occurrence++
		if final && occurrence <= richCardImageMaxCount {
			if _, selectedURL := selected[value]; selectedURL {
				if imageKey, ok := p.richCardImageKey(value); ok {
					images = append(images, ordinaryPostImage{imageKey: imageKey, alt: alt, rawURL: value})
					return ""
				}
			}
		}
		return ordinaryMarkdownImageFallback(alt, value)
	})
	return resolvedText, images
}

func ordinaryPreviewContentBeforeTable(content string) (string, bool) {
	matches := findMarkdownTablesOutsideCodeBlocks(content)
	if len(matches) == 0 {
		return content, false
	}
	prefix := strings.TrimRight(content[:matches[0].start], "\n")
	if strings.TrimSpace(prefix) == "" {
		return "…", true
	}
	return prefix, true
}

func (p *Platform) sendOrdinaryPreviewStart(ctx context.Context, rc replyContext, content string) (any, error) {
	previewContent, tableBuffered := ordinaryPreviewContentBeforeTable(content)
	prepared, _ := p.prepareOrdinaryPostContent(ctx, previewContent, false)
	msgType := larkim.MsgTypePost
	body := buildOrdinaryPostJSON(prepared, nil)
	requestUUID := uuid.NewString()
	var msgID string
	if p.shouldUseThreadOrReplyAPI(rc) {
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(rc.messageID).
			Body(p.buildOrdinaryReplyMessageReqBody(rc, msgType, body, requestUUID)).
			Build()
		var resp *larkim.ReplyMessageResp
		if err := p.withTransientRetry(ctx, "send ordinary preview", func() error {
			return p.withFreshTenantAccessTokenRetry(ctx, "send ordinary preview", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
				var err error
				resp, err = client.Im.Message.Reply(ctx, req, options...)
				if err != nil {
					return fmt.Errorf("%s: send ordinary preview (reply): %w", p.tag(), err)
				}
				if !resp.Success() {
					return fmt.Errorf("%s: send ordinary preview (reply) code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
				}
				return nil
			})
		}); err != nil {
			return nil, err
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
	} else {
		req := larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.ReceiveIdTypeChatId).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(rc.chatID).
				MsgType(msgType).
				Content(body).
				Uuid(requestUUID).
				Build()).
			Build()
		var resp *larkim.CreateMessageResp
		if err := p.withTransientRetry(ctx, "send ordinary preview", func() error {
			return p.withFreshTenantAccessTokenRetry(ctx, "send ordinary preview", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
				var err error
				resp, err = client.Im.Message.Create(ctx, req, options...)
				if err != nil {
					return fmt.Errorf("%s: send ordinary preview: %w", p.tag(), err)
				}
				if !resp.Success() {
					return fmt.Errorf("%s: send ordinary preview code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
				}
				return nil
			})
		}); err != nil {
			return nil, err
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
	}
	if msgID == "" {
		return nil, fmt.Errorf("%s: send ordinary preview: no message ID returned", p.tag())
	}
	return &feishuPreviewHandle{
		kind:                  feishuPreviewKindOrdinary,
		messageID:             msgID,
		chatID:                rc.chatID,
		msgType:               msgType,
		lastContent:           content,
		ordinaryTableBuffered: tableBuffered,
	}, nil
}

func (p *Platform) updateOrdinaryPreview(ctx context.Context, h *feishuPreviewHandle, content string, final bool) error {
	if !final {
		if prefix, hasTable := ordinaryPreviewContentBeforeTable(content); hasTable {
			h.mu.Lock()
			alreadyBuffered := h.ordinaryTableBuffered
			h.ordinaryTableBuffered = true
			h.lastContent = content
			h.mu.Unlock()
			if alreadyBuffered {
				return nil
			}

			prepared, _ := p.prepareOrdinaryPostContent(ctx, prefix, false)
			body := buildOrdinaryPostJSON(prepared, nil)
			return p.updateOrdinaryPreviewPost(ctx, h, body, content, false)
		}
	}
	prepared, _ := p.prepareOrdinaryPostContent(ctx, content, false)
	body := buildOrdinaryPostJSON(prepared, nil)
	return p.updateOrdinaryPreviewPost(ctx, h, body, content, final)
}

func (p *Platform) updateOrdinaryPreviewPost(ctx context.Context, h *feishuPreviewHandle, body, originalContent string, final bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !final && (h.ordinaryFinalized || h.ordinaryFinalizing != nil) {
		h.lastContent = originalContent
		return nil
	}
	if !final && h.ordinarySuccessfulPUTs >= ordinaryPreviewIntermediatePUTLimit {
		h.lastContent = originalContent
		return nil
	}
	if final && h.ordinarySuccessfulPUTs >= ordinaryPreviewTotalPUTLimit {
		return errOrdinaryFinalPUTUnavailable
	}
	if !ordinaryUpdatePostWithinLimits(body) {
		if final {
			return fmt.Errorf("%w: serialized final post exceeds safe limit", errOrdinaryFinalPUTUnavailable)
		}
		h.lastContent = originalContent
		return nil
	}

	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(h.messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType(larkim.MsgTypePost).
			Content(body).
			Build()).
		Build()

	err := p.withTransientRetry(ctx, "update ordinary message", func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, "update ordinary message", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Update(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: update ordinary message: %w", p.tag(), err)
			}
			if !resp.Success() {
				return &ordinaryMessageAPIError{operation: p.tag() + ": update ordinary message", code: resp.Code, msg: resp.Msg}
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	h.ordinarySuccessfulPUTs++
	h.lastContent = originalContent
	return nil
}

func ordinaryUpdatePostWithinLimits(body string) bool {
	if len(body) > ordinaryPostContentPayloadLimit {
		return false
	}
	reqBody := larkim.NewUpdateMessageReqBodyBuilder().
		MsgType(larkim.MsgTypePost).
		Content(body).
		Build()
	return serializedJSONSize(reqBody) <= ordinaryPostRequestBodyLimit
}

func (p *Platform) buildOrdinaryReplyMessageReqBody(rc replyContext, msgType, content, requestUUID string) *larkim.ReplyMessageReqBody {
	body := p.buildReplyMessageReqBody(rc, msgType, content)
	body.Uuid = &requestUUID
	return body
}

func (p *Platform) ordinaryPostChunkWithinLimits(rc replyContext, chunk ordinaryPostChunk) bool {
	body := buildOrdinaryPostJSONWithFooter(chunk.markdown, chunk.images, chunk.footer)
	if len(body) > ordinaryPostContentPayloadLimit || !ordinaryUpdatePostWithinLimits(body) {
		return false
	}

	var requestBody any
	if p.shouldUseThreadOrReplyAPI(rc) {
		requestBody = p.buildOrdinaryReplyMessageReqBody(rc, larkim.MsgTypePost, body, ordinaryRequestUUIDPlaceholder)
	} else {
		requestBody = larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(rc.chatID).
			MsgType(larkim.MsgTypePost).
			Content(body).
			Uuid(ordinaryRequestUUIDPlaceholder).
			Build()
	}
	return serializedJSONSize(requestBody) <= ordinaryPostRequestBodyLimit
}

func serializedJSONSize(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ordinaryPostRequestBodyLimit + 1
	}
	return len(encoded)
}

func (p *Platform) splitOrdinaryPostText(rc replyContext, text string) ([]ordinaryPostChunk, error) {
	candidate := ordinaryPostChunk{markdown: text}
	if p.ordinaryPostChunkWithinLimits(rc, candidate) {
		return []ordinaryPostChunk{candidate}, nil
	}

	runeCount := utf8.RuneCountInString(text)
	if runeCount <= 1 {
		return nil, fmt.Errorf("%s: one-rune post cannot fit serialized request limit", p.tag())
	}

	// A 4,000-rune slice is safely below the byte budget even when every rune
	// is JSON-escaped. Preserve every source rune for ordinary prose/tables;
	// when fenced code is present, prefer the shared splitter so each message
	// closes and reopens the fence and remains independently renderable.
	parts := splitOrdinaryMarkdown(text, ordinaryTextChunkRuneLimit)
	if len(parts) < 2 {
		runes := []rune(text)
		midpoint := runeCount / 2
		parts = []string{string(runes[:midpoint]), string(runes[midpoint:])}
	}

	chunks := make([]ordinaryPostChunk, 0, len(parts))
	for _, part := range parts {
		partChunks, err := p.splitOrdinaryPostText(rc, part)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, partChunks...)
	}
	return chunks, nil
}

func splitOrdinaryMarkdown(text string, maxRunes int) []string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	if strings.Contains(text, "```") {
		return core.SplitMessageCodeFenceAware(text, maxRunes)
	}

	runes := []rune(text)
	chunks := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for len(runes) > maxRunes {
		cut := maxRunes
		for i := maxRunes - 1; i >= 0; i-- {
			if runes[i] == '\n' {
				cut = i + 1 // Keep the source newline instead of losing it between messages.
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

func (p *Platform) buildOrdinaryFinalChunks(rc replyContext, text, footer string, images []ordinaryPostImage) ([]ordinaryPostChunk, error) {
	chunks, err := p.splitOrdinaryPostText(rc, text)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		chunks = []ordinaryPostChunk{{}}
	}

	for _, image := range images {
		last := len(chunks) - 1
		candidate := chunks[last]
		candidate.images = append(append([]ordinaryPostImage(nil), candidate.images...), image)
		if p.ordinaryPostChunkWithinLimits(rc, candidate) {
			chunks[last] = candidate
			continue
		}
		candidate = ordinaryPostChunk{images: []ordinaryPostImage{image}}
		if !p.ordinaryPostChunkWithinLimits(rc, candidate) {
			return nil, fmt.Errorf("%s: image post row cannot fit serialized request limit", p.tag())
		}
		chunks = append(chunks, candidate)
	}

	if strings.TrimSpace(footer) != "" {
		last := len(chunks) - 1
		candidate := chunks[last]
		candidate.markdown = strings.TrimRight(candidate.markdown, "\n")
		candidate.footer = footer
		if p.ordinaryPostChunkWithinLimits(rc, candidate) {
			chunks[last] = candidate
		} else {
			candidate = ordinaryPostChunk{footer: footer}
			if !p.ordinaryPostChunkWithinLimits(rc, candidate) {
				return nil, fmt.Errorf("%s: status footer cannot fit serialized request limit", p.tag())
			}
			chunks = append(chunks, candidate)
		}
	}

	for i := range chunks {
		chunks[i].body = buildOrdinaryPostJSONWithFooter(chunks[i].markdown, chunks[i].images, chunks[i].footer)
		if !p.ordinaryPostChunkWithinLimits(rc, chunks[i]) {
			return nil, fmt.Errorf("%s: post chunk %d exceeds serialized request limit", p.tag(), i)
		}
	}
	return chunks, nil
}

// FinalizePreview implements core.PreviewFinalizer for Hermes ordinary
// messages. One goroutine owns final delivery; concurrent and repeated calls
// observe that result rather than issuing duplicate PUT/POST requests. handled
// is true only after the complete final response has been delivered.
func (p *Platform) FinalizePreview(ctx context.Context, replyCtx, previewHandle any, finalText, statusFooter string) (bool, error) {
	h, ok := previewHandle.(*feishuPreviewHandle)
	if !ok {
		return false, fmt.Errorf("%s: invalid preview handle type %T", p.tag(), previewHandle)
	}
	if h.kind != feishuPreviewKindOrdinary {
		return false, nil
	}

	h.mu.Lock()
	if h.ordinaryFinalized {
		err := h.ordinaryFinalizeErr
		h.mu.Unlock()
		return err == nil, err
	}
	if wait := h.ordinaryFinalizing; wait != nil {
		h.mu.Unlock()
		select {
		case <-wait:
			h.mu.Lock()
			err := h.ordinaryFinalizeErr
			h.mu.Unlock()
			return err == nil, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	h.ordinaryFinalizing = make(chan struct{})
	h.mu.Unlock()

	finalErr := p.finalizeOrdinaryPreview(ctx, replyCtx, h, finalText, statusFooter)

	h.mu.Lock()
	h.ordinaryFinalized = true
	h.ordinaryFinalizeErr = finalErr
	close(h.ordinaryFinalizing)
	h.ordinaryFinalizing = nil
	h.mu.Unlock()
	return finalErr == nil, finalErr
}

func (p *Platform) finalizeOrdinaryPreview(ctx context.Context, replyCtx any, h *feishuPreviewHandle, finalText, statusFooter string) error {
	rc, ok := replyCtx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: ordinary final: invalid reply context type %T", p.tag(), replyCtx)
	}

	finalText = p.resolveMentionsInContent(ctx, rc.chatID, finalText)
	if hasFeishuMentionTag(finalText) {
		completeText := appendOrdinaryStatusFooter(finalText, statusFooter)
		messageIDs, err := p.sendOrdinaryTextChunks(ctx, rc, completeText)
		if err != nil {
			p.deleteOrdinaryMessagesBestEffort(ctx, messageIDs, "clean up partial ordinary mention replacement")
			return fmt.Errorf("%s: send ordinary mention final: %w", p.tag(), err)
		}
		if deleteErr := p.DeletePreviewMessage(ctx, h); deleteErr != nil {
			slog.Warn(p.tag()+": delete stale post preview after mention final failed", "error", deleteErr)
		}
		return nil
	}

	preparedText, images := p.prepareOrdinaryPostContent(ctx, finalText, true)
	chunks, err := p.buildOrdinaryFinalChunks(rc, preparedText, statusFooter, images)
	if err != nil {
		return err
	}

	if putErr := p.updateOrdinaryPreviewPost(ctx, h, chunks[0].body, chunks[0].markdown, true); putErr != nil {
		slog.Warn(p.tag()+": final ordinary preview PUT failed or unavailable; sending complete fallback", "error", putErr)
		messageIDs, fallbackErr := p.sendOrdinaryPostChunks(ctx, rc, chunks)
		if fallbackErr != nil {
			p.deleteOrdinaryMessagesBestEffort(ctx, messageIDs, "clean up partial ordinary preview fallback")
			return fmt.Errorf("%s: ordinary preview final PUT failed (%v), fallback failed: %w", p.tag(), putErr, fallbackErr)
		}
		if deleteErr := p.DeletePreviewMessage(ctx, h); deleteErr != nil {
			slog.Warn(p.tag()+": delete stale ordinary preview after complete fallback failed", "error", deleteErr)
		}
		return nil
	}

	if len(chunks) > 1 {
		messageIDs, overflowErr := p.sendOrdinaryPostChunks(ctx, rc, chunks[1:])
		if overflowErr != nil {
			p.deleteOrdinaryMessagesBestEffort(ctx, messageIDs, "clean up partial ordinary final overflow")
			return fmt.Errorf("%s: send ordinary final overflow: %w", p.tag(), overflowErr)
		}
	}
	return nil
}

func splitOrdinaryTextMentionAware(content string, maxRunes int) ([]string, error) {
	if maxRunes <= 0 {
		return nil, errors.New("ordinary text chunk rune limit must be positive")
	}
	if content == "" {
		return []string{""}, nil
	}

	chunks := make([]string, 0, (utf8.RuneCountInString(content)+maxRunes-1)/maxRunes)
	var current strings.Builder
	currentRunes := 0
	flush := func() {
		if currentRunes == 0 {
			return
		}
		chunks = append(chunks, current.String())
		current.Reset()
		currentRunes = 0
	}
	appendPlain := func(text string) {
		runes := []rune(text)
		for len(runes) > 0 {
			if currentRunes == maxRunes {
				flush()
			}
			take := min(maxRunes-currentRunes, len(runes))
			current.WriteString(string(runes[:take]))
			currentRunes += take
			runes = runes[take:]
		}
	}

	last := 0
	for _, match := range feishuMentionTagPattern.FindAllStringIndex(content, -1) {
		appendPlain(content[last:match[0]])
		tag := content[match[0]:match[1]]
		tagRunes := utf8.RuneCountInString(tag)
		if tagRunes > maxRunes {
			return nil, fmt.Errorf("Feishu mention tag exceeds %d-rune text limit", maxRunes)
		}
		if currentRunes+tagRunes > maxRunes {
			flush()
		}
		current.WriteString(tag)
		currentRunes += tagRunes
		last = match[1]
	}
	appendPlain(content[last:])
	flush()
	return chunks, nil
}

func (p *Platform) sendOrdinaryTextChunks(ctx context.Context, rc replyContext, content string) ([]string, error) {
	chunks, err := splitOrdinaryTextMentionAware(content, ordinaryTextChunkRuneLimit)
	if err != nil {
		return nil, fmt.Errorf("%s: split ordinary text chunks: %w", p.tag(), err)
	}
	messageIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		body, err := json.Marshal(map[string]string{"text": chunk})
		if err != nil {
			return messageIDs, fmt.Errorf("%s: marshal ordinary text chunk: %w", p.tag(), err)
		}
		messageID, err := p.sendOrdinaryTypedMessageWithID(ctx, rc, larkim.MsgTypeText, string(body), "send ordinary final text")
		if err != nil {
			return messageIDs, err
		}
		messageIDs = append(messageIDs, messageID)
	}
	return messageIDs, nil
}

func (p *Platform) sendOrdinaryPostChunks(ctx context.Context, rc replyContext, chunks []ordinaryPostChunk) ([]string, error) {
	messageIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		messageID, err := p.sendOrdinaryPostChunk(ctx, rc, chunk)
		if err != nil {
			return messageIDs, err
		}
		messageIDs = append(messageIDs, messageID)
	}
	return messageIDs, nil
}

// sendOrdinaryPostChunk deliberately bypasses Send so every normal final chunk
// remains a post. Only Feishu's explicit invalid-post-content response permits
// a content-preserving text downgrade.
func (p *Platform) sendOrdinaryPostChunk(ctx context.Context, rc replyContext, chunk ordinaryPostChunk) (string, error) {
	messageID, err := p.sendOrdinaryTypedMessageWithID(ctx, rc, larkim.MsgTypePost, chunk.body, "send ordinary final post")
	if err == nil || !isInvalidPostContentError(err) {
		return messageID, err
	}

	fallbackBody, marshalErr := json.Marshal(map[string]string{"text": chunk.readableFallback()})
	if marshalErr != nil {
		return "", fmt.Errorf("%s: marshal ordinary final text fallback: %w", p.tag(), marshalErr)
	}
	return p.sendOrdinaryTypedMessageWithID(ctx, rc, larkim.MsgTypeText, string(fallbackBody), "send ordinary final text fallback")
}

func (p *Platform) sendOrdinaryTypedMessage(ctx context.Context, rc replyContext, msgType, content, operation string) error {
	_, err := p.sendOrdinaryTypedMessageWithID(ctx, rc, msgType, content, operation)
	return err
}

func (p *Platform) sendOrdinaryTypedMessageWithID(ctx context.Context, rc replyContext, msgType, content, operation string) (string, error) {
	requestUUID := uuid.NewString()
	if p.shouldUseThreadOrReplyAPI(rc) {
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(rc.messageID).
			Body(p.buildOrdinaryReplyMessageReqBody(rc, msgType, content, requestUUID)).
			Build()
		var resp *larkim.ReplyMessageResp
		if err := p.withTransientRetry(ctx, operation, func() error {
			return p.withFreshTenantAccessTokenRetry(ctx, operation, func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
				var err error
				resp, err = client.Im.Message.Reply(ctx, req, options...)
				if err != nil {
					return fmt.Errorf("%s: %s api call: %w", p.tag(), operation, err)
				}
				if !resp.Success() {
					return &ordinaryMessageAPIError{operation: p.tag() + ": " + operation, code: resp.Code, msg: resp.Msg}
				}
				return nil
			})
		}); err != nil {
			return "", err
		}
		if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
			return "", fmt.Errorf("%s: %s: no message ID returned", p.tag(), operation)
		}
		return *resp.Data.MessageId, nil
	}

	if rc.chatID == "" {
		return "", fmt.Errorf("%s: chatID is empty, cannot %s", p.tag(), operation)
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(rc.chatID).
			MsgType(msgType).
			Content(content).
			Uuid(requestUUID).
			Build()).
		Build()
	var resp *larkim.CreateMessageResp
	if err := p.withTransientRetry(ctx, operation, func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, operation, func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			var err error
			resp, err = client.Im.Message.Create(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: %s api call: %w", p.tag(), operation, err)
			}
			if !resp.Success() {
				return &ordinaryMessageAPIError{operation: p.tag() + ": " + operation, code: resp.Code, msg: resp.Msg}
			}
			return nil
		})
	}); err != nil {
		return "", err
	}
	if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
		return "", fmt.Errorf("%s: %s: no message ID returned", p.tag(), operation)
	}
	return *resp.Data.MessageId, nil
}

func (p *Platform) deleteOrdinaryMessagesBestEffort(ctx context.Context, messageIDs []string, operation string) {
	for i := len(messageIDs) - 1; i >= 0; i-- {
		if err := p.deleteMessageByID(ctx, messageIDs[i], operation); err != nil {
			slog.Warn(p.tag()+": "+operation+" failed", "message_id", messageIDs[i], "error", err)
		}
	}
}
