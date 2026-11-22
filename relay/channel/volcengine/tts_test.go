package volcengine

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

type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (r *flushSignalRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.flushed) })
}

// A client that disconnects after audio was delivered is charged the input
// upstream synthesized; a disconnect before any audio is refunded.
func TestTTSWebSocketDisconnectSettlesDeliveredAudio(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		sendAudio bool
	}{
		{name: "disconnect after delivered audio charges the input", sendAudio: true},
		{name: "disconnect before any audio refunds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requested := make(chan struct{})
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
				close(requested)
				if tc.sendAudio {
					msg, err := NewMessage(MsgTypeAudioOnlyServer, MsgTypeFlagPositiveSeq)
					require.NoError(t, err)
					msg.Sequence, msg.Payload = 1, []byte("partial-audio")
					frame, err := msg.Marshal()
					require.NoError(t, err)
					_ = conn.WriteMessage(websocket.BinaryMessage, frame)
				}
				<-release
			}))
			defer server.Close()
			defer close(release)

			recorder := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
			c, _ := gin.CreateTestContext(recorder)
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil).WithContext(requestCtx)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "app|token"}}
			info.SetEstimatePromptTokens(12)
			go func() {
				<-requested
				if tc.sendAudio {
					<-recorder.flushed
				}
				cancelRequest()
			}()

			usage, apiErr := handleTTSWebSocketResponse(c, "ws"+strings.TrimPrefix(server.URL, "http"), VolcengineTTSRequest{}, info, "mp3")
			if !tc.sendAudio {
				require.NotNil(t, apiErr)
				return
			}
			require.Nil(t, apiErr)
			require.IsType(t, &dto.Usage{}, usage)
			assert.Equal(t, 12, usage.(*dto.Usage).PromptTokens)
			assert.Equal(t, "partial-audio", recorder.Body.String())
		})
	}
}
