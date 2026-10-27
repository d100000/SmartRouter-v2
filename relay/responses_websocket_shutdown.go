package relay

import (
	"context"
	"sync"

	"github.com/gorilla/websocket"
)

type responsesShutdownRegistry struct {
	mu       sync.Mutex
	stopping bool
	sessions map[*responsesWSSession]chan struct{}
}

var responsesShutdown = &responsesShutdownRegistry{sessions: make(map[*responsesWSSession]chan struct{})}

// Unregister runs only after the session's request workers and their deferred
// terminal observations finish. net/http does not drain hijacked connections.
func registerResponsesShutdown(session *responsesWSSession) func() {
	registry := responsesShutdown
	done := make(chan struct{})
	registry.mu.Lock()
	registry.sessions[session] = done
	stopping := registry.stopping
	registry.mu.Unlock()
	if stopping {
		session.shutdown(websocket.CloseGoingAway, "server shutting down")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.mu.Lock()
			delete(registry.sessions, session)
			close(done)
			registry.mu.Unlock()
		})
	}
}

func ShutdownResponsesWebSockets(ctx context.Context) error {
	registry := responsesShutdown
	registry.mu.Lock()
	registry.stopping = true
	sessions := make(map[*responsesWSSession]chan struct{}, len(registry.sessions))
	for session, done := range registry.sessions {
		sessions[session] = done
	}
	registry.mu.Unlock()
	for session := range sessions {
		go session.shutdown(websocket.CloseGoingAway, "server shutting down")
	}
	for _, done := range sessions {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
