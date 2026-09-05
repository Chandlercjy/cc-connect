package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The platform deliberately ignores cancellation until the test releases it:
// cancellation is not proof that an already-started delivery/save has finished.
type shutdownBlockingPlatform struct {
	stubPlatformEngine
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *shutdownBlockingPlatform) deliver(text string) error {
	if strings.Contains(text, "persist-before-stop") {
		p.once.Do(func() { close(p.entered) })
		<-p.release
	}
	return p.stubPlatformEngine.Send(context.Background(), nil, text)
}
func (p *shutdownBlockingPlatform) Send(_ context.Context, _ any, text string) error {
	return p.deliver(text)
}
func (p *shutdownBlockingPlatform) Reply(_ context.Context, _ any, text string) error {
	return p.deliver(text)
}

func awaitShutdownSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestEngineStop_WaitsForDeliveryAndPersistence(t *testing.T) {
	for _, unsolicited := range []bool{false, true} {
		name := "foreground"
		if unsolicited {
			name = "unsolicited"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			p := &shutdownBlockingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, entered: make(chan struct{}), release: make(chan struct{})}
			as := newResultAgentSession("persist-before-stop")
			e := NewEngine("test", &resultAgent{session: as}, []Platform{p}, filepath.Join(dir, "sessions.json"), LangEnglish)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(p.release) }) }
			t.Cleanup(func() { release(); _ = e.Stop() })
			session := e.sessions.GetOrCreateActive("test:shutdown")
			var readerDone <-chan struct{}
			if unsolicited {
				state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
				e.interactiveStates["test:shutdown"] = state
				e.startUnsolicitedReader(state, session, e.sessions, "test:shutdown", "")
				state.mu.Lock()
				readerDone = state.unsolicitedDone
				state.mu.Unlock()
				as.events <- Event{Type: EventResult, Content: "persist-before-stop", Done: true}
			} else {
				e.ReceiveMessage(p, &Message{SessionKey: "test:shutdown", Platform: "test", MessageID: "s1", Content: "hello", ReplyCtx: "ctx"})
			}
			awaitShutdownSignal(t, p.entered, "blocked platform delivery")
			stopped := make(chan struct{})
			go func() { _ = e.Stop(); close(stopped) }()
			awaitShutdownSignal(t, e.ctx.Done(), "shutdown cancellation")
			early := false
			select {
			case <-stopped:
				early = true
			case <-time.After(50 * time.Millisecond):
			}
			release()
			awaitShutdownSignal(t, stopped, "Stop completion")
			if readerDone != nil {
				awaitShutdownSignal(t, readerDone, "reader completion")
			}
			if early {
				t.Errorf("Stop returned while platform delivery and subsequent persistence were still blocked")
			}
			data, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
			if err != nil || !strings.Contains(string(data), "persist-before-stop") {
				t.Errorf("Stop did not finish persistence: %v, %s", err, data)
			}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			e.ReceiveMessage(p, &Message{SessionKey: "test:late", Platform: "test", Content: "/new", ReplyCtx: "ctx"})
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("late message recreated state after Stop: %v", err)
			}
		})
	}
}

type shutdownStartingAgent struct {
	stubAgent
	entered chan struct{}
	stopped chan struct{}
	release chan struct{}
	once    sync.Once
	session *shutdownStartSession
}

func (a *shutdownStartingAgent) StartSession(context.Context, string) (AgentSession, error) {
	close(a.entered)
	<-a.stopped // Stop must unblock the agent before taking interactiveMu.
	<-a.release
	return a.session, nil
}
func (a *shutdownStartingAgent) Stop() error { a.once.Do(func() { close(a.stopped) }); return nil }

type shutdownStartSession struct {
	stubAgentSession
	closed atomic.Bool
	sends  atomic.Int32
}

func (s *shutdownStartSession) Close() error { s.closed.Store(true); return nil }
func (s *shutdownStartSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	s.sends.Add(1)
	return nil
}
func TestEngineStop_DuringSessionStart(t *testing.T) {
	a := &shutdownStartingAgent{entered: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{}), session: &shutdownStartSession{}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.ReceiveMessage(p, &Message{SessionKey: "test:start", Platform: "test", Content: "start", ReplyCtx: "ctx"})
	awaitShutdownSignal(t, a.entered, "session start")
	stopped := make(chan struct{})
	go func() { _ = e.Stop(); close(stopped) }()
	// On the old implementation this reports the lock inversion without leaving
	// the blocked startup goroutine behind during TempDir cleanup.
	select {
	case <-a.stopped:
	case <-time.After(200 * time.Millisecond):
		t.Error("Stop waited for interactiveMu before unblocking Agent.StartSession")
		_ = a.Stop()
	}
	select {
	case <-stopped:
		t.Error("Stop returned before startup settled")
	default:
	}
	close(a.release)
	awaitShutdownSignal(t, stopped, "Stop after startup")
	if !a.session.closed.Load() {
		t.Error("late-started session was not closed")
	}
	if a.session.sends.Load() != 0 {
		t.Error("message sent to session that started during Stop")
	}
	session := e.sessions.GetOrCreateActive("test:start")
	if !session.TryLock() {
		t.Error("startup early return leaked session lock")
	} else {
		session.Unlock()
	}
}

func TestEngineStop_RejectsNewInbound(t *testing.T) {
	for _, cancelOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "cancelled"}[cancelOnly], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(dir, "sessions.json"), LangEnglish)
			if cancelOnly {
				e.cancel()
			} else {
				_ = e.Stop()
			}
			e.ReceiveMessage(p, &Message{SessionKey: "test:late", Platform: "test", Content: "/new", ReplyCtx: "ctx"})
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("closed engine created state: %v", err)
			}
			if len(p.getSent()) != 0 {
				t.Error("closed engine replied to new inbound message")
			}
		})
	}
}
