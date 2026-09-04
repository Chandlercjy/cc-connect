package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type ordinaryMessageRequest struct {
	method        string
	path          string
	msgType       string
	content       string
	requestUUID   string
	replyInThread bool
	rawBodyBytes  int
}

type ordinaryMessageRecorder struct {
	mu                    sync.Mutex
	requests              []ordinaryMessageRequest
	putCode               int
	putMsg                string
	postCode              int
	postMsg               string
	directPostCode        int
	directPostMsg         string
	textCode              int
	textMsg               string
	transientPostFailures int
	postFailureAt         int
	textFailureAt         int
	postAttempts          int
	textAttempts          int
	successfulMessages    int
	sawPUT                bool
	putGate               <-chan struct{}
	putStarted            chan struct{}
	putOnce               sync.Once
}

func (r *ordinaryMessageRecorder) record(req ordinaryMessageRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if req.method == http.MethodPut {
		r.sawPUT = true
	}
}

func (r *ordinaryMessageRecorder) responseFailure(req ordinaryMessageRequest) (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.method == http.MethodPut && r.putCode != 0 {
		msg := r.putMsg
		if msg == "" {
			msg = "forced update failure"
		}
		return r.putCode, msg
	}
	if r.sawPUT && req.method == http.MethodPost && req.msgType == larkim.MsgTypePost {
		r.postAttempts++
		if r.postFailureAt > 0 && r.postAttempts == r.postFailureAt {
			return 230001, "forced post failure"
		}
		if r.postCode != 0 {
			msg := r.postMsg
			if msg == "" {
				msg = "forced post failure"
			}
			return r.postCode, msg
		}
	}
	if req.method == http.MethodPost && req.msgType == larkim.MsgTypePost && r.directPostCode != 0 {
		msg := r.directPostMsg
		if msg == "" {
			msg = "forced direct post failure"
		}
		return r.directPostCode, msg
	}
	if req.method == http.MethodPost && req.msgType == larkim.MsgTypeText {
		r.textAttempts++
		if r.textFailureAt > 0 && r.textAttempts == r.textFailureAt {
			return 230001, "forced text failure"
		}
		if r.textCode != 0 {
			msg := r.textMsg
			if msg == "" {
				msg = "forced text failure"
			}
			return r.textCode, msg
		}
	}
	return 0, ""
}

func (r *ordinaryMessageRecorder) nextMessageID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.successfulMessages++
	if r.successfulMessages == 1 {
		return "om_preview"
	}
	return fmt.Sprintf("om_new_%d", r.successfulMessages-1)
}

func (r *ordinaryMessageRecorder) consumeTransientPostFailure(req ordinaryMessageRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.method != http.MethodPost || r.transientPostFailures == 0 {
		return false
	}
	r.transientPostFailures--
	return true
}

func (r *ordinaryMessageRecorder) snapshot() []ordinaryMessageRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ordinaryMessageRequest(nil), r.requests...)
}

func newOrdinaryMessageTestPlatform(t *testing.T, useCards bool, recorder *ordinaryMessageRecorder) (*Platform, func()) {
	t.Helper()
	const appID = "cli_ordinary_message"
	const appSecret = "secret"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeJSON(t, w, map[string]any{
				"code":                0,
				"expire":              7200,
				"tenant_access_token": "tenant-token",
			})
			return
		}

		var body struct {
			MsgType       string `json:"msg_type"`
			Content       string `json:"content"`
			UUID          string `json:"uuid"`
			ReplyInThread bool   `json:"reply_in_thread"`
		}
		var raw []byte
		if req.Body != nil {
			var err error
			raw, err = io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
		}
		recorded := ordinaryMessageRequest{
			method:        req.Method,
			path:          req.URL.Path,
			msgType:       body.MsgType,
			content:       body.Content,
			requestUUID:   body.UUID,
			replyInThread: body.ReplyInThread,
			rawBodyBytes:  len(raw),
		}
		recorder.record(recorded)
		if recorder.consumeTransientPostFailure(recorded) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatalf("response writer does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack failed: %v", err)
			}
			_ = conn.Close()
			return
		}
		if req.Method == http.MethodPut {
			if recorder.putStarted != nil {
				recorder.putOnce.Do(func() { close(recorder.putStarted) })
			}
			if recorder.putGate != nil {
				<-recorder.putGate
			}
		}

		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/open-apis/cardkit/v1/cards":
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"card_id": "card_entity_1"}})
		case func() bool {
			code, msg := recorder.responseFailure(recorded)
			if code == 0 {
				return false
			}
			writeJSON(t, w, map[string]any{"code": code, "msg": msg})
			return true
		}():
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/reply"):
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": recorder.nextMessageID()}})
		case req.Method == http.MethodPost && req.URL.Path == "/open-apis/im/v1/messages":
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": recorder.nextMessageID()}})
		default:
			writeJSON(t, w, map[string]any{"code": 0, "msg": "success"})
		}
	}))

	p := &Platform{
		platformName:        "feishu",
		domain:              srv.URL,
		appID:               appID,
		appSecret:           appSecret,
		ordinaryMessageMode: "hermes",
		useInteractiveCard:  useCards,
		client: lark.NewClient(appID, appSecret,
			lark.WithOpenBaseUrl(srv.URL),
			lark.WithHttpClient(srv.Client()),
		),
		replayClient: lark.NewClient(appID, appSecret,
			lark.WithEnableTokenCache(false),
			lark.WithOpenBaseUrl(srv.URL),
			lark.WithHttpClient(srv.Client()),
		),
	}
	return p, srv.Close
}

func TestLegacyOrdinaryModeKeepsMarkdownCardBehavior(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()
	p.ordinaryMessageMode = ordinaryMessageModeLegacy

	if err := p.Reply(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, "**legacy markdown**"); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != 1 || requests[0].msgType != larkim.MsgTypeInteractive {
		t.Fatalf("legacy markdown requests = %#v, want existing interactive-card path", requests)
	}
}

func TestHermesOrdinaryReplyUsesTextForPlainAndPostMDForMarkdown(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	if err := p.Reply(context.Background(), rctx, "plain reply"); err != nil {
		t.Fatalf("plain Reply() error = %v", err)
	}
	if err := p.Reply(context.Background(), rctx, "## Result\n\n| A | B |\n|---|---|\n| 1 | 2 |"); err != nil {
		t.Fatalf("markdown Reply() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2: %#v", len(requests), requests)
	}
	if requests[0].msgType != larkim.MsgTypeText || !strings.Contains(requests[0].content, "plain reply") {
		t.Fatalf("plain request = %#v, want text message", requests[0])
	}
	if requests[1].msgType != larkim.MsgTypePost || !strings.Contains(requests[1].content, `"tag":"md"`) {
		t.Fatalf("markdown request = %#v, want post+md", requests[1])
	}
	if requests[1].msgType == larkim.MsgTypeInteractive {
		t.Fatalf("ordinary markdown must not be cardified: %#v", requests[1])
	}
}

func TestHermesOrdinaryDirectReplyUploadsRemoteImages(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()
	p.richCardImageUploadFunc = func(context.Context, string) (string, error) {
		return "img_v3_direct", nil
	}

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	if err := p.Reply(context.Background(), rctx, "chart\n![result](https://example.com/result.png)"); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1: %#v", len(requests), requests)
	}
	if requests[0].msgType != larkim.MsgTypePost {
		t.Fatalf("msg_type = %q, want post", requests[0].msgType)
	}
	if got := postImageKeys(t, requests[0].content); len(got) != 1 || got[0] != "img_v3_direct" {
		t.Fatalf("image keys = %v, want img_v3_direct", got)
	}
	if strings.Contains(postMarkdownText(t, requests[0].content), "https://example.com/result.png") {
		t.Fatalf("uploaded remote image URL remained in markdown: %s", requests[0].content)
	}
}

func TestHermesOrdinaryDirectPostUsesCompleteByteSafeChunks(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	finalText := "# Result\n\n" + strings.Repeat("\"\\中", 9000) + "终点"
	if err := p.Reply(context.Background(), rctx, finalText); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) < 2 {
		t.Fatalf("request count = %d, want multiple post chunks", len(requests))
	}
	var rebuilt strings.Builder
	for i, req := range requests {
		if req.method != http.MethodPost || req.msgType != larkim.MsgTypePost {
			t.Fatalf("chunk %d = %#v, want POST post", i, req)
		}
		if req.rawBodyBytes > ordinaryPostRequestBodyLimit {
			t.Fatalf("chunk %d serialized request = %d, limit %d", i, req.rawBodyBytes, ordinaryPostRequestBodyLimit)
		}
		if req.requestUUID == "" {
			t.Fatalf("chunk %d missing request UUID", i)
		}
		rebuilt.WriteString(postMarkdownText(t, req.content))
	}
	if got := rebuilt.String(); got != finalText {
		t.Fatalf("rebuilt direct post length = %d, want complete %d-byte response", len(got), len(finalText))
	}
}

func TestHermesOrdinaryDirectPostDowngradesOnlyExplicitContentErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code         int
		msg          string
		wantErr      bool
		wantTextSend bool
	}{
		{name: "real Feishu content error independent of code", code: 230099, msg: "content format of the post type is incorrect", wantTextSend: true},
		{name: "generic rate limit", code: 230001, msg: "send too fast, please retry later", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{directPostCode: tc.code, directPostMsg: tc.msg}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()

			err := p.Reply(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, "full **answer**")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Reply() error = %v, wantErr %v", err, tc.wantErr)
			}
			requests := recorder.snapshot()
			var textRequests []ordinaryMessageRequest
			for _, req := range requests {
				if req.msgType == larkim.MsgTypeText {
					textRequests = append(textRequests, req)
				}
			}
			if (len(textRequests) > 0) != tc.wantTextSend {
				t.Fatalf("text fallback requests = %#v, wantTextSend %v; all=%#v", textRequests, tc.wantTextSend, requests)
			}
		})
	}
}

func TestHermesOrdinaryPreviewStartsAsPostAndUsesPUT(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	handleAny, err := p.SendPreviewStart(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, "plain first frame")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	handle, ok := handleAny.(*feishuPreviewHandle)
	if !ok {
		t.Fatalf("handle type = %T, want *feishuPreviewHandle", handleAny)
	}
	if handle.kind != feishuPreviewKindOrdinary || handle.msgType != larkim.MsgTypePost {
		t.Fatalf("handle = kind %q msgType %q, want ordinary/post", handle.kind, handle.msgType)
	}
	if err := p.UpdateMessage(context.Background(), handle, "second frame with **markdown**"); err != nil {
		t.Fatalf("UpdateMessage() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2: %#v", len(requests), requests)
	}
	if requests[0].method != http.MethodPost || requests[0].msgType != larkim.MsgTypePost {
		t.Fatalf("first preview request = %#v, want POST post", requests[0])
	}
	if requests[1].method != http.MethodPut || requests[1].path != "/open-apis/im/v1/messages/om_preview" {
		t.Fatalf("preview update request = %#v, want SDK PUT on same message", requests[1])
	}
	if requests[1].msgType != larkim.MsgTypePost || !strings.Contains(requests[1].content, `"tag":"md"`) {
		t.Fatalf("preview PUT = %#v, want post+md", requests[1])
	}
}

func TestHermesOrdinaryTableFreezesAfterPrefixAndFinalizesInOriginalPost(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handleAny, err := p.SendPreviewStart(context.Background(), rctx, "Intro par")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	table := "Intro paragraph complete.\n\n| Name | Value |\n|---|---|\n| alpha | 1 |"
	if err := p.UpdateMessage(context.Background(), handleAny, table); err != nil {
		t.Fatalf("UpdateMessage(table) error = %v", err)
	}
	if err := p.UpdateMessage(context.Background(), handleAny, table+"\n| beta | 2 |"); err != nil {
		t.Fatalf("UpdateMessage(table extension) error = %v", err)
	}
	final := table + "\n| beta | 2 |\n\nDone."
	handled, err := p.FinalizePreview(context.Background(), rctx, handleAny, final, "model · ctx")
	if err != nil || !handled {
		t.Fatalf("FinalizePreview() = handled %v err %v", handled, err)
	}

	requests := recorder.snapshot()
	var putBodies []string
	var freshPostCount, deleteCount int
	for i, req := range requests {
		switch {
		case req.method == http.MethodPut:
			putBodies = append(putBodies, postMarkdownText(t, req.content))
		case i > 0 && req.method == http.MethodPost:
			freshPostCount++
		case req.method == http.MethodDelete:
			deleteCount++
		}
	}
	if len(putBodies) != 2 {
		t.Fatalf("table PUT count = %d, want prefix + final PUT; requests=%#v", len(putBodies), requests)
	}
	if putBodies[0] != "Intro paragraph complete." || strings.Contains(putBodies[0], "| Name |") {
		t.Fatalf("prefix PUT = %q, want complete table-free prefix", putBodies[0])
	}
	if !strings.Contains(putBodies[1], final) || !strings.Contains(putBodies[1], "model · ctx") {
		t.Fatalf("final PUT = %q, want complete table and footer", putBodies[1])
	}
	if freshPostCount != 0 || deleteCount != 0 {
		t.Fatalf("fresh posts=%d deletes=%d, want original preview updated without recall; requests=%#v", freshPostCount, deleteCount, requests)
	}
}

func TestHermesOrdinaryTableInFirstFrameUsesPlaceholderThenFinalPUT(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	table := "| Name | Value |\n|---|---|\n| alpha | 1 |"
	handleAny, err := p.SendPreviewStart(context.Background(), rctx, table)
	if err != nil {
		t.Fatalf("SendPreviewStart(table) error = %v", err)
	}
	handle := handleAny.(*feishuPreviewHandle)
	if !handle.ordinaryTableBuffered {
		t.Fatal("table in first frame should mark preview as locally buffered")
	}
	requests := recorder.snapshot()
	if len(requests) != 1 || strings.Contains(postMarkdownText(t, requests[0].content), "| Name |") {
		t.Fatalf("first table frame should send a table-free placeholder: %#v", requests)
	}

	handled, err := p.FinalizePreview(context.Background(), rctx, handle, table, "")
	if err != nil || !handled {
		t.Fatalf("FinalizePreview() = handled %v err %v", handled, err)
	}
	requests = recorder.snapshot()
	var putBodies []string
	var freshPostCount, deleteCount int
	for i, req := range requests {
		if req.method == http.MethodPut {
			putBodies = append(putBodies, postMarkdownText(t, req.content))
		}
		if i > 0 && req.method == http.MethodPost {
			freshPostCount++
		}
		if req.method == http.MethodDelete {
			deleteCount++
		}
	}
	if len(putBodies) != 1 || !strings.Contains(putBodies[0], "| alpha | 1 |") {
		t.Fatalf("final PUT bodies=%v, want one complete table update", putBodies)
	}
	if freshPostCount != 0 || deleteCount != 0 {
		t.Fatalf("fresh posts=%d deletes=%d, want original placeholder updated without recall; requests=%#v", freshPostCount, deleteCount, requests)
	}
}

func TestHermesOrdinaryPOSTRequestUUIDIsPresentAndStableAcrossRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		preview bool
		reply   bool
	}{
		{name: "preview reply", preview: true, reply: true},
		{name: "preview create", preview: true, reply: false},
		{name: "typed reply", preview: false, reply: true},
		{name: "typed create", preview: false, reply: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{transientPostFailures: 1}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()

			rctx := replyContext{chatID: "oc_chat"}
			if tc.reply {
				rctx.messageID = "om_trigger"
			}
			var err error
			if tc.preview {
				_, err = p.sendOrdinaryPreviewStart(context.Background(), rctx, "preview")
			} else {
				body, marshalErr := json.Marshal(map[string]string{"text": "final"})
				if marshalErr != nil {
					t.Fatalf("marshal text body: %v", marshalErr)
				}
				err = p.sendOrdinaryTypedMessage(context.Background(), rctx, larkim.MsgTypeText, string(body), "test ordinary UUID")
			}
			if err != nil {
				t.Fatalf("ordinary send error = %v", err)
			}

			requests := recorder.snapshot()
			if len(requests) < 2 {
				t.Fatalf("request count = %d, want initial attempt plus retry: %#v", len(requests), requests)
			}
			wantUUID := requests[0].requestUUID
			if _, err := uuid.Parse(wantUUID); err != nil {
				t.Fatalf("request UUID = %q, want valid UUID: %v", wantUUID, err)
			}
			if len(wantUUID) != len(ordinaryRequestUUIDPlaceholder) {
				t.Fatalf("request UUID length = %d, placeholder length = %d", len(wantUUID), len(ordinaryRequestUUIDPlaceholder))
			}
			for i, req := range requests[1:] {
				if req.requestUUID != wantUUID {
					t.Fatalf("retry %d UUID = %q, want stable %q", i+1, req.requestUUID, wantUUID)
				}
			}
		})
	}
}

func TestHermesOrdinaryPreviewReservesTwentiethPUTForFinalize(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	handleAny, err := p.SendPreviewStart(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, "frame 0")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	for i := 1; i <= 25; i++ {
		if err := p.UpdateMessage(context.Background(), handleAny, strings.Repeat("x", i)); err != nil {
			t.Fatalf("intermediate UpdateMessage(%d) error = %v", i, err)
		}
	}
	handled, err := p.FinalizePreview(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, handleAny, "final answer", "footer")
	if err != nil {
		t.Fatalf("FinalizePreview() error = %v", err)
	}
	if !handled {
		t.Fatal("FinalizePreview() handled = false, want true for ordinary preview")
	}

	requests := recorder.snapshot()
	putRequests := make([]ordinaryMessageRequest, 0)
	for _, req := range requests {
		if req.method == http.MethodPut {
			putRequests = append(putRequests, req)
		}
	}
	if len(putRequests) != 20 {
		t.Fatalf("PUT count = %d, want 19 intermediate + 1 final", len(putRequests))
	}
	last := putRequests[len(putRequests)-1]
	if !strings.Contains(last.content, "final answer") || !strings.Contains(last.content, "footer") {
		t.Fatalf("final PUT content = %q, want final answer and inline footer", last.content)
	}
}

func TestHermesOrdinaryFinalizePUTFailureFallsBackOnceWithOriginalReplyContext(t *testing.T) {
	recorder := &ordinaryMessageRecorder{putCode: 230001}
	p, closeServer := newOrdinaryMessageTestPlatform(t, false, recorder)
	defer closeServer()

	p.threadIsolation = true
	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat", sessionKey: "feishu:oc_chat:root:om_root"}
	handleAny, err := p.SendPreviewStart(context.Background(), rctx, "first frame")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	finalText := strings.Repeat("\"\\中", 8000)
	handled, err := p.FinalizePreview(context.Background(), rctx, handleAny, finalText, "")
	if err != nil {
		t.Fatalf("FinalizePreview() fallback error = %v", err)
	}
	if !handled {
		t.Fatal("FinalizePreview() handled = false, want true after owned fallback")
	}

	requests := recorder.snapshot()
	var putCount, deleteCount, replyCount int
	var rebuilt strings.Builder
	seenFinalPUT := false
	lastFallbackIndex := -1
	deleteIndex := -1
	for i, req := range requests {
		switch {
		case req.method == http.MethodPut:
			putCount++
			seenFinalPUT = true
		case req.method == http.MethodDelete:
			deleteCount++
			deleteIndex = i
		case seenFinalPUT && req.method == http.MethodPost && req.path == "/open-apis/im/v1/messages/om_trigger/reply":
			replyCount++
			lastFallbackIndex = i
			if !req.replyInThread {
				t.Fatalf("fallback reply lost reply_in_thread context: %#v", req)
			}
			if req.msgType != larkim.MsgTypePost {
				t.Fatalf("fallback chunk type = %q, want fixed post", req.msgType)
			}
			rebuilt.WriteString(postMarkdownText(t, req.content))
		}
	}
	if putCount != 1 {
		t.Fatalf("final PUT count = %d, want exactly 1", putCount)
	}
	if deleteCount != 1 {
		t.Fatalf("stale preview delete count = %d, want 1 after fallback", deleteCount)
	}
	if replyCount < 2 {
		t.Fatalf("fallback reply count = %d, want multiple byte-safe chunks", replyCount)
	}
	if deleteIndex <= lastFallbackIndex {
		t.Fatalf("delete index = %d, last fallback index = %d; preview must be deleted only after complete fallback", deleteIndex, lastFallbackIndex)
	}
	if rebuilt.String() != finalText {
		t.Fatalf("fallback rebuilt length = %d, want complete %d-byte response", rebuilt.Len(), len(finalText))
	}
}

func TestHermesOrdinaryFallbackFailurePreservesPreviewAndReportsUnhandled(t *testing.T) {
	recorder := &ordinaryMessageRecorder{putCode: 230001, postCode: 230001, postMsg: "send too fast, please retry later"}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "partial preview")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	handled, err := p.FinalizePreview(context.Background(), rctx, handle, "complete **final**", "footer")
	if err == nil || handled {
		t.Fatalf("FinalizePreview() = handled %v err %v, want false,error", handled, err)
	}

	requests := recorder.snapshot()
	for _, req := range requests {
		if req.method == http.MethodDelete {
			t.Fatalf("failed replacement deleted the still-useful preview: %#v", requests)
		}
	}
	beforeRepeat := len(requests)
	handled, err = p.FinalizePreview(context.Background(), rctx, handle, "complete **final**", "footer")
	if err == nil || handled {
		t.Fatalf("repeated FinalizePreview() = handled %v err %v, want cached false,error", handled, err)
	}
	if afterRepeat := len(recorder.snapshot()); afterRepeat != beforeRepeat {
		t.Fatalf("repeated failed finalize issued more requests: before=%d after=%d", beforeRepeat, afterRepeat)
	}
}

func TestHermesOrdinaryFallbackSecondChunkFailureCleansNewChunkAndPreservesPreview(t *testing.T) {
	recorder := &ordinaryMessageRecorder{putCode: 230001, postFailureAt: 2}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "partial preview")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	finalText := "# Complete\n\n" + strings.Repeat("\"\\中", 9000)
	handled, err := p.FinalizePreview(context.Background(), rctx, handle, finalText, "footer")
	if err == nil || handled {
		t.Fatalf("FinalizePreview() = handled %v err %v, want false,error", handled, err)
	}

	requests := recorder.snapshot()
	firstChunkIndex, failedChunkIndex, cleanupIndex := -1, -1, -1
	for i, req := range requests {
		switch {
		case req.method == http.MethodPost && req.msgType == larkim.MsgTypePost && firstChunkIndex < 0 && i > 1:
			firstChunkIndex = i
		case req.method == http.MethodPost && req.msgType == larkim.MsgTypePost && firstChunkIndex >= 0:
			failedChunkIndex = i
		case req.method == http.MethodDelete && req.path == "/open-apis/im/v1/messages/om_new_1":
			cleanupIndex = i
		case req.method == http.MethodDelete && req.path == "/open-apis/im/v1/messages/om_preview":
			t.Fatalf("failed fallback deleted original preview: %#v", requests)
		}
	}
	if firstChunkIndex < 0 || failedChunkIndex <= firstChunkIndex || cleanupIndex <= failedChunkIndex {
		t.Fatalf("request order first=%d failed=%d cleanup=%d; requests=%#v", firstChunkIndex, failedChunkIndex, cleanupIndex, requests)
	}
}

func TestHermesOrdinaryOverflowSecondChunkFailureCleansOverflowAndKeepsUpdatedPreview(t *testing.T) {
	recorder := &ordinaryMessageRecorder{postFailureAt: 2}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "partial preview")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	finalText := "# Complete\n\n" + strings.Repeat("\"\\中", 9000)
	handled, err := p.FinalizePreview(context.Background(), rctx, handle, finalText, "footer")
	if err == nil || handled {
		t.Fatalf("FinalizePreview() = handled %v err %v, want false,error", handled, err)
	}

	requests := recorder.snapshot()
	putIndex, firstOverflowIndex, failedOverflowIndex, cleanupIndex := -1, -1, -1, -1
	for i, req := range requests {
		switch {
		case req.method == http.MethodPut:
			putIndex = i
		case req.method == http.MethodPost && putIndex >= 0 && firstOverflowIndex < 0:
			firstOverflowIndex = i
		case req.method == http.MethodPost && firstOverflowIndex >= 0:
			failedOverflowIndex = i
		case req.method == http.MethodDelete && req.path == "/open-apis/im/v1/messages/om_new_1":
			cleanupIndex = i
		case req.method == http.MethodDelete && req.path == "/open-apis/im/v1/messages/om_preview":
			t.Fatalf("failed overflow deleted updated preview first segment: %#v", requests)
		}
	}
	if putIndex < 0 || firstOverflowIndex <= putIndex || failedOverflowIndex <= firstOverflowIndex || cleanupIndex <= failedOverflowIndex {
		t.Fatalf("request order PUT=%d first=%d failed=%d cleanup=%d; requests=%#v", putIndex, firstOverflowIndex, failedOverflowIndex, cleanupIndex, requests)
	}
}

func TestHermesOrdinaryFooterAndStatusStayNonCard(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, false, recorder)
	defer closeServer()

	handleAny, err := p.SendPreviewStart(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, "first")
	if err != nil {
		t.Fatalf("SendPreviewStart() with cards disabled error = %v", err)
	}
	if !p.KeepPreviewOnFinish(handleAny) {
		t.Fatal("ordinary preview should be kept on finish even when cards are disabled")
	}
	p.SetPreviewStatus(handleAny, core.CardStatusDone)
	if err := p.UpdateMessageWithStatusFooter(context.Background(), handleAny, "body", "model · ctx"); err != nil {
		t.Fatalf("UpdateMessageWithStatusFooter() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want start + footer PUT only: %#v", len(requests), requests)
	}
	last := requests[len(requests)-1]
	if last.method != http.MethodPut || last.msgType != larkim.MsgTypePost {
		t.Fatalf("footer update = %#v, want ordinary post PUT", last)
	}
	if strings.Contains(last.content, `"schema":"2.0"`) || strings.Contains(last.content, `"msg_type":"interactive"`) {
		t.Fatalf("ordinary footer was cardified: %s", last.content)
	}
}

func TestIsHermesCardJSONRequiresCard2BodyElementsStructure(t *testing.T) {
	built := (&Platform{}).BuildRichCard(core.CardStatusDone, "", nil, "answer", false, "")
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "BuildRichCard output", content: built, want: true},
		{name: "ordinary lookalike", content: `{"schema":"x","body":"example"}`},
		{name: "wrong schema", content: `{"schema":"1.0","body":{"elements":[]}}`},
		{name: "body not object", content: `{"schema":"2.0","body":"example"}`},
		{name: "elements missing", content: `{"schema":"2.0","body":{}}`},
		{name: "elements not array", content: `{"schema":"2.0","body":{"elements":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHermesCardJSON(tc.content); got != tc.want {
				t.Fatalf("isHermesCardJSON(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestHermesOrdinaryJSONLookalikeIsNotSentInteractive(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	content := `{"schema":"x","body":"example"}`
	if err := p.Reply(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, content); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	requests := recorder.snapshot()
	if len(requests) != 1 || requests[0].msgType == larkim.MsgTypeInteractive {
		t.Fatalf("requests = %#v, ordinary JSON lookalike must not use interactive", requests)
	}
}

func TestHermesCardsDisabledRichCardUsesReadableOrdinaryPost(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, false, recorder)
	defer closeServer()

	cardJSON := `{"schema":"2.0","body":{"elements":[{"tag":"column_set","columns":[{"tag":"column","elements":[{"tag":"markdown","content":"nested **answer**"}]}]},{"tag":"markdown","content":"tail"}]}}`
	if err := p.SendWithStatusFooter(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, cardJSON, "model · ctx"); err != nil {
		t.Fatalf("SendWithStatusFooter() error = %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1: %#v", len(requests), requests)
	}
	if requests[0].msgType != larkim.MsgTypePost {
		t.Fatalf("msg_type = %q, want ordinary post", requests[0].msgType)
	}
	text := postMarkdownText(t, requests[0].content)
	for _, want := range []string{"nested **answer**", "tail", "model · ctx"} {
		if !strings.Contains(text, want) {
			t.Fatalf("ordinary post text = %q, want %q", text, want)
		}
	}
	if strings.Contains(text, `"schema":"2.0"`) {
		t.Fatalf("readable card downgrade leaked raw card JSON despite markdown content: %q", text)
	}
}

func TestHermesCardsDisabledCardWithoutMarkdownPreservesRawContentAsText(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, false, recorder)
	defer closeServer()

	cardJSON := `{"schema":"2.0","body":{"elements":[{"tag":"hr"}]}}`
	if err := p.Reply(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, cardJSON); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	requests := recorder.snapshot()
	if len(requests) != 1 || requests[0].msgType != larkim.MsgTypeText {
		t.Fatalf("requests = %#v, want one ordinary text message", requests)
	}
	if got := textMessageText(t, requests[0].content); got != cardJSON {
		t.Fatalf("raw card fallback = %q, want original %q", got, cardJSON)
	}
}

func TestHermesCardsDisabledProgressPayloadUsesReadableOrdinaryMessage(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, false, recorder)
	defer closeServer()

	progress := core.BuildProgressCardPayloadV2([]core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: "planning next step"}}, false, "Codex", core.LangEnglish, core.ProgressCardStateRunning)
	if err := p.Reply(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, progress); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	requests := recorder.snapshot()
	if len(requests) != 1 || requests[0].msgType == larkim.MsgTypeInteractive {
		t.Fatalf("requests = %#v, cards-disabled progress must be ordinary", requests)
	}
	var text string
	if requests[0].msgType == larkim.MsgTypePost {
		text = postMarkdownText(t, requests[0].content)
	} else {
		text = textMessageText(t, requests[0].content)
	}
	if !strings.Contains(text, "planning next step") {
		t.Fatalf("progress downgrade text = %q, want readable progress", text)
	}
}

func TestHermesPrebuiltRichPreviewKeepsCardKitPath(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	cardJSON := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"answer"}]}}`
	handleAny, err := p.SendPreviewStart(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, cardJSON)
	if err != nil {
		t.Fatalf("SendPreviewStart(rich card) error = %v", err)
	}
	handle := handleAny.(*feishuPreviewHandle)
	if handle.kind != feishuPreviewKindCard || handle.msgType != larkim.MsgTypeInteractive || handle.cardID != "card_entity_1" {
		t.Fatalf("rich handle = kind %q msgType %q cardID %q, want card/interactive/card_entity_1", handle.kind, handle.msgType, handle.cardID)
	}
	if !p.KeepPreviewOnFinish(handle) {
		t.Fatal("card preview should retain legacy keep-on-finish behavior")
	}
	if err := p.UpdateMessage(context.Background(), handle, cardJSON); err != nil {
		t.Fatalf("UpdateMessage(rich card) error = %v", err)
	}

	requests := recorder.snapshot()
	var createdEntity, sentInteractive, updatedEntity bool
	for _, req := range requests {
		switch {
		case req.method == http.MethodPost && req.path == "/open-apis/cardkit/v1/cards":
			createdEntity = true
		case req.method == http.MethodPost && strings.HasSuffix(req.path, "/reply") && req.msgType == larkim.MsgTypeInteractive:
			sentInteractive = true
		case req.method == http.MethodPut && req.path == "/open-apis/cardkit/v1/cards/card_entity_1":
			updatedEntity = true
		}
	}
	if !createdEntity || !sentInteractive || !updatedEntity {
		t.Fatalf("rich card path = create:%v send:%v update:%v; requests=%#v", createdEntity, sentInteractive, updatedEntity, requests)
	}
}

func TestHermesInteractivePreviewStillUsesCardPath(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	progress := core.BuildProgressCardPayloadV2([]core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: "planning"}}, false, "Codex", core.LangEnglish, core.ProgressCardStateRunning)
	handleAny, err := p.SendPreviewStart(context.Background(), replyContext{messageID: "om_trigger", chatID: "oc_chat"}, progress)
	if err != nil {
		t.Fatalf("SendPreviewStart(progress) error = %v", err)
	}
	handle := handleAny.(*feishuPreviewHandle)
	if handle.kind != feishuPreviewKindCard || handle.msgType != larkim.MsgTypeInteractive {
		t.Fatalf("progress handle = kind %q msgType %q, want card/interactive", handle.kind, handle.msgType)
	}

	requests := recorder.snapshot()
	if len(requests) != 1 || requests[0].msgType != larkim.MsgTypeInteractive {
		t.Fatalf("progress requests = %#v, want interactive card reply", requests)
	}
}

func postRows(t *testing.T, content string) [][]map[string]any {
	t.Helper()
	var post struct {
		ZH struct {
			Content [][]map[string]any `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(content), &post); err != nil {
		t.Fatalf("unmarshal post content: %v; content=%q", err, content)
	}
	return post.ZH.Content
}

func postMarkdownText(t *testing.T, content string) string {
	t.Helper()
	var b strings.Builder
	for _, row := range postRows(t, content) {
		for _, elem := range row {
			if elem["tag"] == "md" {
				if text, ok := elem["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
	}
	return b.String()
}

func postImageKeys(t *testing.T, content string) []string {
	t.Helper()
	var keys []string
	for _, row := range postRows(t, content) {
		for _, elem := range row {
			if elem["tag"] == "img" {
				if key, ok := elem["image_key"].(string); ok {
					keys = append(keys, key)
				}
			}
		}
	}
	return keys
}

func textMessageText(t *testing.T, content string) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal([]byte(content), &body); err != nil {
		t.Fatalf("unmarshal text message content: %v; content=%q", err, content)
	}
	return body["text"]
}

func TestHermesOrdinaryResolvedMentionFinalUsesFixedTextChunksThenDeletesPostPreview(t *testing.T) {
	for _, tc := range []struct {
		name            string
		noReply         bool
		wantPath        string
		wantThreadReply bool
	}{
		{name: "reply in thread", wantPath: "/open-apis/im/v1/messages/om_trigger/reply", wantThreadReply: true},
		{name: "no reply to trigger", noReply: true, wantPath: "/open-apis/im/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()
			p.resolveMentions = true
			p.threadIsolation = true
			p.noReplyToTrigger = tc.noReply
			p.chatMemberCache.Store("oc_chat", &chatMemberEntry{
				members:   map[string]string{"Alice": "ou_alice"},
				fetchedAt: time.Now(),
			})

			rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat", sessionKey: "feishu:oc_chat:root:om_root"}
			handle, err := p.SendPreviewStart(context.Background(), rctx, "sticky post preview")
			if err != nil {
				t.Fatalf("SendPreviewStart() error = %v", err)
			}
			finalText := "@Alice " + strings.Repeat("中", 8000)
			const footer = "model · ctx"
			handled, err := p.FinalizePreview(context.Background(), rctx, handle, finalText, footer)
			if err != nil || !handled {
				t.Fatalf("FinalizePreview() = handled %v err %v", handled, err)
			}

			requests := recorder.snapshot()
			var rebuilt strings.Builder
			textCount := 0
			lastTextIndex, deleteIndex := -1, -1
			for i, req := range requests {
				switch {
				case req.method == http.MethodPut:
					t.Fatalf("resolved mention crossed preview from post to text via PUT: %#v", requests)
				case req.method == http.MethodPost && req.msgType == larkim.MsgTypeText:
					textCount++
					lastTextIndex = i
					if req.path != tc.wantPath || req.replyInThread != tc.wantThreadReply {
						t.Fatalf("text chunk = %#v, want path %q thread=%v", req, tc.wantPath, tc.wantThreadReply)
					}
					text := textMessageText(t, req.content)
					if got := utf8.RuneCountInString(text); got > ordinaryTextChunkRuneLimit {
						t.Fatalf("text chunk runes = %d, limit %d", got, ordinaryTextChunkRuneLimit)
					}
					rebuilt.WriteString(text)
				case req.method == http.MethodDelete:
					deleteIndex = i
				}
			}
			if textCount < 2 {
				t.Fatalf("text chunk count = %d, want multiple 4000-rune chunks", textCount)
			}
			want := appendOrdinaryStatusFooter(`<at user_id="ou_alice">Alice</at> `+strings.Repeat("中", 8000), footer)
			if got := rebuilt.String(); got != want {
				t.Fatalf("rebuilt mention final runes = %d, want complete %d", utf8.RuneCountInString(got), utf8.RuneCountInString(want))
			}
			if deleteIndex <= lastTextIndex {
				t.Fatalf("delete index = %d, last text index = %d; preview must remain until all text chunks succeed", deleteIndex, lastTextIndex)
			}
		})
	}
}

func TestHermesOrdinaryMentionTagsStayAtomicAtChunkBoundary(t *testing.T) {
	for _, tag := range []string{
		`<at user_id="ou_alice">Alice</at>`,
		`<at id=all>everyone</at>`,
	} {
		t.Run(tag, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()

			rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
			handle, err := p.SendPreviewStart(context.Background(), rctx, "sticky post preview")
			if err != nil {
				t.Fatalf("SendPreviewStart() error = %v", err)
			}
			finalText := strings.Repeat("x", ordinaryTextChunkRuneLimit-10) + tag + strings.Repeat("y", 30)
			handled, err := p.FinalizePreview(context.Background(), rctx, handle, finalText, "")
			if err != nil || !handled {
				t.Fatalf("FinalizePreview() = handled %v err %v", handled, err)
			}

			var rebuilt strings.Builder
			tagChunkCount := 0
			for _, req := range recorder.snapshot() {
				if req.method != http.MethodPost || req.msgType != larkim.MsgTypeText {
					continue
				}
				text := textMessageText(t, req.content)
				if utf8.RuneCountInString(text) > ordinaryTextChunkRuneLimit {
					t.Fatalf("text chunk exceeded %d runes: %d", ordinaryTextChunkRuneLimit, utf8.RuneCountInString(text))
				}
				if strings.Contains(text, tag) {
					tagChunkCount++
				}
				rebuilt.WriteString(text)
			}
			if tagChunkCount != 1 {
				t.Fatalf("complete mention tag appeared in %d text chunks, want exactly 1", tagChunkCount)
			}
			if rebuilt.String() != finalText {
				t.Fatalf("rebuilt text differs from final content")
			}
		})
	}
}

func TestHermesOrdinaryResolvedMentionFailurePreservesPostPreview(t *testing.T) {
	recorder := &ordinaryMessageRecorder{textCode: 230001, textMsg: "send too fast, please retry later"}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()
	p.resolveMentions = true
	p.chatMemberCache.Store("oc_chat", &chatMemberEntry{
		members:   map[string]string{"Alice": "ou_alice"},
		fetchedAt: time.Now(),
	})

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "sticky post preview")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	handled, err := p.FinalizePreview(context.Background(), rctx, handle, "@Alice complete final", "footer")
	if err == nil || handled {
		t.Fatalf("FinalizePreview() = handled %v err %v, want false,error", handled, err)
	}
	for _, req := range recorder.snapshot() {
		if req.method == http.MethodPut || req.method == http.MethodDelete {
			t.Fatalf("failed mention replacement must preserve sticky post preview: %#v", recorder.snapshot())
		}
	}
}

func TestHermesOrdinaryMentionSecondChunkFailureCleansNewChunkAndPreservesPreview(t *testing.T) {
	recorder := &ordinaryMessageRecorder{textFailureAt: 2}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()
	p.resolveMentions = true
	p.chatMemberCache.Store("oc_chat", &chatMemberEntry{
		members:   map[string]string{"Alice": "ou_alice"},
		fetchedAt: time.Now(),
	})

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "sticky post preview")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	handled, err := p.FinalizePreview(context.Background(), rctx, handle, "@Alice "+strings.Repeat("中", 8000), "footer")
	if err == nil || handled {
		t.Fatalf("FinalizePreview() = handled %v err %v, want false,error", handled, err)
	}

	requests := recorder.snapshot()
	firstChunkIndex, failedChunkIndex, cleanupIndex := -1, -1, -1
	for i, req := range requests {
		switch {
		case req.method == http.MethodPost && req.msgType == larkim.MsgTypeText && firstChunkIndex < 0:
			firstChunkIndex = i
		case req.method == http.MethodPost && req.msgType == larkim.MsgTypeText:
			failedChunkIndex = i
		case req.method == http.MethodDelete && req.path == "/open-apis/im/v1/messages/om_new_1":
			cleanupIndex = i
		case req.method == http.MethodDelete && req.path == "/open-apis/im/v1/messages/om_preview":
			t.Fatalf("failed mention replacement deleted original preview: %#v", requests)
		}
	}
	if firstChunkIndex < 0 || failedChunkIndex <= firstChunkIndex || cleanupIndex <= failedChunkIndex {
		t.Fatalf("request order first=%d failed=%d cleanup=%d; requests=%#v", firstChunkIndex, failedChunkIndex, cleanupIndex, requests)
	}
}

func TestHermesOrdinaryFinalChunksUseSerializedByteLimitsAndFooterOnlyOnLast(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "frame")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	finalText := strings.Repeat("\"\\中", 8000) + "终点"
	const footer = "footer-only-on-last"
	if handled, err := p.FinalizePreview(context.Background(), rctx, handle, finalText, footer); err != nil || !handled {
		t.Fatalf("FinalizePreview() = handled %v err %v", handled, err)
	}

	requests := recorder.snapshot()
	var delivery []ordinaryMessageRequest
	for _, req := range requests[1:] {
		if req.method == http.MethodPut || req.method == http.MethodPost {
			delivery = append(delivery, req)
		}
	}
	if len(delivery) < 2 || delivery[0].method != http.MethodPut {
		t.Fatalf("final delivery = %#v, want first PUT plus overflow POST", delivery)
	}
	var rebuilt strings.Builder
	footerCount := 0
	for i, req := range delivery {
		if req.msgType != larkim.MsgTypePost {
			t.Fatalf("chunk %d msg_type = %q, want post", i, req.msgType)
		}
		if req.rawBodyBytes > ordinaryPostRequestBodyLimit {
			t.Fatalf("chunk %d serialized request = %d, limit %d", i, req.rawBodyBytes, ordinaryPostRequestBodyLimit)
		}
		if len(req.content) > ordinaryPostContentPayloadLimit {
			t.Fatalf("chunk %d post payload = %d, limit %d", i, len(req.content), ordinaryPostContentPayloadLimit)
		}
		text := postMarkdownText(t, req.content)
		if !utf8.ValidString(text) {
			t.Fatalf("chunk %d split invalid UTF-8", i)
		}
		footerCount += strings.Count(text, footer)
		if i != len(delivery)-1 && strings.Contains(text, footer) {
			t.Fatalf("footer appeared before final chunk %d", i)
		}
		rebuilt.WriteString(text)
	}
	if footerCount != 1 {
		t.Fatalf("footer occurrences = %d, want 1", footerCount)
	}
	if got, want := rebuilt.String(), finalText+"\n\n"+footer; got != want {
		t.Fatalf("rebuilt final length = %d, want %d complete bytes", len(got), len(want))
	}
}

func TestHermesOrdinaryFinalOverflowPreservesReplyAndNoReplySemantics(t *testing.T) {
	for _, tc := range []struct {
		name            string
		noReply         bool
		wantPath        string
		wantThreadReply bool
	}{
		{name: "reply in thread", wantPath: "/open-apis/im/v1/messages/om_trigger/reply", wantThreadReply: true},
		{name: "no reply to trigger", noReply: true, wantPath: "/open-apis/im/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()
			p.threadIsolation = true
			p.noReplyToTrigger = tc.noReply
			rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat", sessionKey: "feishu:oc_chat:root:om_root"}
			handle, err := p.SendPreviewStart(context.Background(), rctx, "frame")
			if err != nil {
				t.Fatalf("SendPreviewStart() error = %v", err)
			}
			if _, err := p.FinalizePreview(context.Background(), rctx, handle, strings.Repeat("\\\"中", 9000), ""); err != nil {
				t.Fatalf("FinalizePreview() error = %v", err)
			}

			requests := recorder.snapshot()
			var overflow []ordinaryMessageRequest
			seenPUT := false
			for _, req := range requests {
				if req.method == http.MethodPut {
					seenPUT = true
					continue
				}
				if seenPUT && req.method == http.MethodPost {
					overflow = append(overflow, req)
				}
			}
			if len(overflow) == 0 {
				t.Fatal("expected final overflow POSTs")
			}
			for _, req := range overflow {
				if req.path != tc.wantPath || req.replyInThread != tc.wantThreadReply {
					t.Fatalf("overflow request = %#v, want path %q thread=%v", req, tc.wantPath, tc.wantThreadReply)
				}
			}
		})
	}
}

func TestHermesOrdinaryExhaustedTwentiethPUTUsesCompleteFallback(t *testing.T) {
	recorder := &ordinaryMessageRecorder{}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()

	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handleAny, err := p.SendPreviewStart(context.Background(), rctx, "frame")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}
	handle := handleAny.(*feishuPreviewHandle)
	handle.ordinarySuccessfulPUTs = ordinaryPreviewTotalPUTLimit
	if _, err := p.FinalizePreview(context.Background(), rctx, handle, "complete final", "footer"); err != nil {
		t.Fatalf("FinalizePreview() fallback error = %v", err)
	}

	requests := recorder.snapshot()
	var puts, deletes int
	var fallback *ordinaryMessageRequest
	for i := range requests {
		req := &requests[i]
		if req.method == http.MethodPut {
			puts++
		}
		if req.method == http.MethodDelete {
			deletes++
		}
		if req.method == http.MethodPost && strings.HasSuffix(req.path, "/reply") && req.msgType == larkim.MsgTypePost && i > 0 {
			fallback = req
		}
	}
	if puts != 0 || deletes != 1 || fallback == nil {
		t.Fatalf("budget fallback puts=%d deletes=%d fallback=%#v requests=%#v", puts, deletes, fallback, requests)
	}
	if got := postMarkdownText(t, fallback.content); got != "complete final\n\nfooter" {
		t.Fatalf("fallback content = %q", got)
	}
}

func TestBuildOrdinaryPostJSONSplitsFencedCodeIntoMultipleRows(t *testing.T) {
	body := buildOrdinaryPostJSON("before\n```go\nfmt.Println(1)\n```\nafter", nil)
	rows := postRows(t, body)
	if len(rows) != 3 {
		t.Fatalf("post rows = %d, want prose/code/prose: %#v", len(rows), rows)
	}
	got := []string{
		rows[0][0]["text"].(string),
		rows[1][0]["text"].(string),
		rows[2][0]["text"].(string),
	}
	want := []string{"before", "```go\nfmt.Println(1)\n```", "after"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHermesOrdinaryFinalRemoteImagesUseRowsAndVisibleFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		upload       func(context.Context, string) (string, error)
		wantImageKey string
		wantText     string
		unwantedText string
	}{
		{
			name: "uploaded image row",
			upload: func(context.Context, string) (string, error) {
				return "img_v3_chart", nil
			},
			wantImageKey: "img_v3_chart",
			unwantedText: "https://example.com/chart.png",
		},
		{
			name: "failed upload clickable fallback",
			upload: func(context.Context, string) (string, error) {
				return "", errors.New("upload failed")
			},
			wantText: "[chart](https://example.com/chart.png)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()
			p.richCardImageUploadFunc = tc.upload
			rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
			handle, err := p.SendPreviewStart(context.Background(), rctx, "frame")
			if err != nil {
				t.Fatalf("SendPreviewStart() error = %v", err)
			}
			final := "before\n![chart](https://example.com/chart.png)\nafter\n![local](/tmp/chart.png)"
			const footer = "model · ctx"
			if _, err := p.FinalizePreview(context.Background(), rctx, handle, final, footer); err != nil {
				t.Fatalf("FinalizePreview() error = %v", err)
			}
			requests := recorder.snapshot()
			put := requests[len(requests)-1]
			text := postMarkdownText(t, put.content)
			keys := postImageKeys(t, put.content)
			if tc.wantImageKey != "" && (len(keys) != 1 || keys[0] != tc.wantImageKey) {
				t.Fatalf("image keys = %v, want %q", keys, tc.wantImageKey)
			}
			if tc.wantText != "" && !strings.Contains(text, tc.wantText) {
				t.Fatalf("post text = %q, want visible fallback %q", text, tc.wantText)
			}
			if tc.unwantedText != "" && strings.Contains(text, tc.unwantedText) {
				t.Fatalf("uploaded URL remained in text: %q", text)
			}
			if !strings.Contains(text, "local (/tmp/chart.png)") {
				t.Fatalf("non-remote image path disappeared: %q", text)
			}
			if !strings.Contains(text, footer) {
				t.Fatalf("status footer disappeared: %q", text)
			}
			if tc.wantImageKey != "" {
				imageRow, footerRow := -1, -1
				for i, row := range postRows(t, put.content) {
					for _, elem := range row {
						switch elem["tag"] {
						case "img":
							imageRow = i
						case "md":
							if value, _ := elem["text"].(string); strings.Contains(value, footer) {
								footerRow = i
							}
						}
					}
				}
				if imageRow < 0 || footerRow <= imageRow {
					t.Fatalf("row order image=%d footer=%d, want footer after image: %s", imageRow, footerRow, put.content)
				}
			}
		})
	}
}

func TestHermesOrdinaryIntermediateImagesDoNotWaitAndFinalCapsOccurrencesWithDedupedUploads(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	started := make(chan struct{})
	release := make(chan struct{})
	var uploadMu sync.Mutex
	uploadCounts := map[string]int{}
	p.richCardImageUploadFunc = func(_ context.Context, rawURL string) (string, error) {
		uploadMu.Lock()
		uploadCounts[rawURL]++
		uploadMu.Unlock()
		if strings.HasSuffix(rawURL, "/1.png") {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
		}
		return "img_" + strings.TrimSuffix(strings.TrimPrefix(rawURL, "https://example.com/"), ".png"), nil
	}

	firstURL := "https://example.com/1.png"
	start := time.Now()
	intermediate, images := p.prepareOrdinaryPostContent(context.Background(), "![one]("+firstURL+")", false)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("intermediate image preparation waited %s", elapsed)
	}
	if len(images) != 0 || !strings.Contains(intermediate, "[one]("+firstURL+")") {
		t.Fatalf("intermediate = %q images=%v, want clickable placeholder and no img row", intermediate, images)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("intermediate preparation did not start shared upload")
	}
	close(release)

	input := strings.Join([]string{
		"![one](https://example.com/1.png)",
		"![two](https://example.com/2.png)",
		"![one duplicate](https://example.com/1.png)",
		"![three](https://example.com/3.png)",
		"![four](https://example.com/4.png)",
		"![five](https://example.com/5.png)",
	}, "\n")
	finalText, finalImages := p.prepareOrdinaryPostContent(context.Background(), input, true)
	if len(finalImages) != richCardImageMaxCount {
		t.Fatalf("final image count = %d, want cap %d", len(finalImages), richCardImageMaxCount)
	}
	wantKeys := []string{"img_1", "img_2", "img_1", "img_3"}
	for i, want := range wantKeys {
		if finalImages[i].imageKey != want {
			t.Fatalf("image occurrence %d key = %q, want stable %q", i, finalImages[i].imageKey, want)
		}
	}
	for _, fallback := range []string{
		"[four](https://example.com/4.png)",
		"[five](https://example.com/5.png)",
	} {
		if !strings.Contains(finalText, fallback) {
			t.Fatalf("over-limit remote image occurrence disappeared: want %q in %q", fallback, finalText)
		}
	}
	if strings.Count(finalText, "https://example.com/1.png") != 0 {
		t.Fatalf("successfully uploaded duplicate URL remained in text: %q", finalText)
	}
	uploadMu.Lock()
	defer uploadMu.Unlock()
	if uploadCounts[firstURL] != 1 {
		t.Fatalf("duplicate URL upload count = %d, want 1", uploadCounts[firstURL])
	}
	if uploadCounts["https://example.com/4.png"] != 0 || uploadCounts["https://example.com/5.png"] != 0 {
		t.Fatalf("over-limit occurrences started uploads: %#v", uploadCounts)
	}
}

func TestHermesOrdinaryInvalidPostDowngradesButGenericErrorDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name         string
		postMsg      string
		wantErr      bool
		wantTextSend bool
	}{
		{name: "explicit invalid post", postMsg: "invalid post content: invalid href", wantTextSend: true},
		{name: "generic API failure", postMsg: "rate limited", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &ordinaryMessageRecorder{putCode: 230001, postCode: 230001, postMsg: tc.postMsg}
			p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
			defer closeServer()
			rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
			handle, err := p.SendPreviewStart(context.Background(), rctx, "frame")
			if err != nil {
				t.Fatalf("SendPreviewStart() error = %v", err)
			}
			handled, err := p.FinalizePreview(context.Background(), rctx, handle, "full **answer**", "footer")
			if (err != nil) != tc.wantErr || handled == tc.wantErr {
				t.Fatalf("FinalizePreview() = handled %v err %v, wantErr %v", handled, err, tc.wantErr)
			}
			requests := recorder.snapshot()
			var textRequests []ordinaryMessageRequest
			for _, req := range requests {
				if req.msgType == larkim.MsgTypeText {
					textRequests = append(textRequests, req)
				}
			}
			if (len(textRequests) > 0) != tc.wantTextSend {
				t.Fatalf("text fallback requests = %#v, wantTextSend %v; all=%#v", textRequests, tc.wantTextSend, requests)
			}
			if tc.wantTextSend {
				var body map[string]string
				if err := json.Unmarshal([]byte(textRequests[0].content), &body); err != nil {
					t.Fatalf("unmarshal text fallback: %v", err)
				}
				if body["text"] != "full **answer**\n\nfooter" {
					t.Fatalf("text fallback lost content: %q", body["text"])
				}
			}
		})
	}
}

func TestHermesOrdinaryConcurrentFinalizeSendsOnlyOnce(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	recorder := &ordinaryMessageRecorder{putGate: gate, putStarted: started}
	p, closeServer := newOrdinaryMessageTestPlatform(t, true, recorder)
	defer closeServer()
	rctx := replyContext{messageID: "om_trigger", chatID: "oc_chat"}
	handle, err := p.SendPreviewStart(context.Background(), rctx, "frame")
	if err != nil {
		t.Fatalf("SendPreviewStart() error = %v", err)
	}

	errs := make(chan error, 2)
	go func() {
		_, err := p.FinalizePreview(context.Background(), rctx, handle, "final", "")
		errs <- err
	}()
	<-started
	go func() {
		_, err := p.FinalizePreview(context.Background(), rctx, handle, "final", "")
		errs <- err
	}()
	close(gate)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("FinalizePreview() error = %v", err)
		}
	}
	if _, err := p.FinalizePreview(context.Background(), rctx, handle, "final", ""); err != nil {
		t.Fatalf("repeated FinalizePreview() error = %v", err)
	}

	putCount := 0
	for _, req := range recorder.snapshot() {
		if req.method == http.MethodPut {
			putCount++
		}
	}
	if putCount != 1 {
		t.Fatalf("final PUT count = %d, want exactly 1", putCount)
	}
}
