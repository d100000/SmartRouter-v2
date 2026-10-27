package helper

import (
	"maps"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

// StreamStartBuffer holds only protocol initialization events. The scanner owns
// its lifetime and serializes ALL operations with its existing write mutex,
// including expiry and heartbeats. No alternate writer or background flusher
// can race an overload decision into a partially committed response.
type StreamStartBuffer struct {
	active    bool
	discarded bool
	deadline  time.Time
	headers   http.Header
	chunks    []streamStartChunk
	bytes     int
}

type streamStartChunk struct{ event, data string }

func (b *StreamStartBuffer) initialize(c *gin.Context) {
	if b == nil || c.Writer.Written() {
		return
	}
	b.active = true
	b.deadline = time.Now().Add(10 * time.Second)
	b.headers = c.Writer.Header().Clone()
}

func (b *StreamStartBuffer) CanRetry(c *gin.Context) bool {
	return b != nil && b.active && !b.discarded && !c.Writer.Written() &&
		c.Request.Context().Err() == nil && time.Now().Before(b.deadline)
}

func (b *StreamStartBuffer) Append(c *gin.Context, event, data string) (bool, error) {
	if !b.CanRetry(c) || len(b.chunks) >= 32 || b.bytes+len(event)+len(data) > 64<<10 {
		return false, b.Flush(c)
	}
	b.chunks = append(b.chunks, streamStartChunk{event: event, data: data})
	b.bytes += len(event) + len(data)
	return true, nil
}

func (b *StreamStartBuffer) Flush(c *gin.Context) error {
	if b == nil || !b.active {
		return nil
	}
	b.active = false
	chunks := b.chunks
	b.chunks = nil
	for _, chunk := range chunks {
		if err := ResponseChunkData(c, dto.ResponsesStreamResponse{Type: chunk.event}, chunk.data); err != nil {
			return err
		}
	}
	return nil
}

// Discard restores the pre-attempt headers as well as removing its events.
// In particular, failed X-Codex-Turn-State values must never escape on retry.
func (b *StreamStartBuffer) Discard(c *gin.Context) bool {
	if !b.CanRetry(c) {
		return false
	}
	b.active, b.discarded = false, true
	b.chunks = nil
	clear(c.Writer.Header())
	maps.Copy(c.Writer.Header(), b.headers)
	return true
}
