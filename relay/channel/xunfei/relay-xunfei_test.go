package xunfei

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disconnectRecorder lets a test observe delivered chunks and close the client.
type disconnectRecorder struct {
	*httptest.ResponseRecorder
	gone      chan bool
	flushed   chan struct{}
	flushOnce sync.Once
}

func (r *disconnectRecorder) CloseNotify() <-chan bool { return r.gone }

func (r *disconnectRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.flushOnce.Do(func() { close(r.flushed) })
}

func TestXunfeiStreamDisconnectSettlesAnsweredGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name          string
		frame         string
		disconnect    string
		wantErr       bool
		wantPrompt    int
		wantGenerated bool
	}{
		{
			name:          "client gone after streamed text without usage charges the local estimate",
			frame:         `{"header":{"code":0,"status":1},"payload":{"choices":{"status":1,"text":[{"content":"hello world from spark","role":"assistant"}]}}}`,
			disconnect:    "client",
			wantPrompt:    7,
			wantGenerated: true,
		},
		{
			name:          "request canceled after a frame with usage charges upstream usage",
			frame:         `{"header":{"code":0,"status":1},"payload":{"choices":{"status":1,"text":[{"content":"hi","role":"assistant"}]},"usage":{"text":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}}}`,
			disconnect:    "context",
			wantPrompt:    11,
			wantGenerated: true,
		},
		{
			name:       "client gone before upstream answered refunds",
			disconnect: "client",
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan struct{})
			release := make(chan struct{})
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				close(received)
				if tt.frame != "" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(tt.frame))
				}
				<-release
			}))
			defer server.Close()
			defer close(release)
			previousHost := xunfeiSparkHost
			xunfeiSparkHost = "ws" + strings.TrimPrefix(server.URL, "http")
			defer func() { xunfeiSparkHost = previousHost }()

			recorder := &disconnectRecorder{
				ResponseRecorder: httptest.NewRecorder(),
				gone:             make(chan bool, 1),
				flushed:          make(chan struct{}),
			}
			c, _ := gin.CreateTestContext(recorder)
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "SparkDesk-v1.1"}}
			info.SetEstimatePromptTokens(7)

			go func() {
				<-received
				if tt.frame != "" {
					<-recorder.flushed
				}
				if tt.disconnect == "context" {
					cancelRequest()
					return
				}
				recorder.gone <- true
			}()

			request := dto.GeneralOpenAIRequest{Model: "SparkDesk-v1.1", Messages: []dto.Message{{Role: "user", Content: "hi"}}}
			usage, apiErr := xunfeiStreamHandler(c, info, request, "app", "secret", "key")
			if tt.wantErr {
				require.NotNil(t, apiErr)
				return
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tt.wantPrompt, usage.PromptTokens)
			assert.Equal(t, tt.wantGenerated, usage.CompletionTokens > 0)
		})
	}
}
