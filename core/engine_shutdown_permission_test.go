package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type shutdownPermissionSession struct {
	*resultAgentSession
	request Event
}

func (s *shutdownPermissionSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	s.events <- s.request
	return nil
}

type shutdownPermissionAgent struct {
	stubAgent
	session AgentSession
}

func (a *shutdownPermissionAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return a.session, nil
}

func TestEngineStop_WhileAwaitingPermissionOrQuestion(t *testing.T) {
	for _, tool := range []string{"Bash", "AskUserQuestion", "extension_input", "extension_select"} {
		t.Run(tool, func(t *testing.T) {
			as := &shutdownPermissionSession{
				resultAgentSession: newResultAgentSession(""),
				request: Event{Type: EventPermissionRequest, RequestID: "shutdown-request", ToolName: tool,
					ToolInput: "test request", Questions: testQuestions()},
			}
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &shutdownPermissionAgent{session: as}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
			key := "test:permission"
			e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", Content: "request", ReplyCtx: "ctx"})
			var pending *pendingPermission
			t.Cleanup(func() {
				if pending != nil {
					pending.resolve()
				}
				_ = e.Stop()
			})
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				_, pending = e.lookupPending(key)
				if pending != nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if pending == nil {
				t.Fatal("permission request was not published")
			}
			stopped := make(chan struct{})
			go func() { _ = e.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(300 * time.Millisecond):
				t.Error("Stop blocked waiting for a human response after cancellation")
				// Unblock the old implementation so a failing regression also cleans up.
				pending.resolve()
				awaitShutdownSignal(t, stopped, "Stop cleanup")
			}
		})
	}
}
