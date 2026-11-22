package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// A generation that ran upstream without usable usage (session ended before
// response.done, or response.done without usage) is charged its reserved input
// plus the output delivered so far, never refunded.
func TestRealtimeEstimatedUsageChargesDeliveredOutput(t *testing.T) {
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-realtime"}}
	for _, tc := range []struct {
		name                       string
		text, transcript           string
		audioBytes, bytesPerSecond int64
		wantText                   string
		wantAudio                  int
		wantSeconds                float64
	}{
		{name: "reservation only", bytesPerSecond: 48000},
		{name: "text deltas", text: "Hello there, how can I help?", bytesPerSecond: 48000, wantText: "Hello there, how can I help?"},
		// 1.5 s of 24 kHz pcm16 at the realtime output-audio rate of
		// service.CountAudioTokenOutput (200 tokens per 0.24 minute).
		{name: "audio with transcript bills audio only", transcript: "spoken words", audioBytes: 72000, bytesPerSecond: 48000, wantAudio: 20, wantSeconds: 1.5},
		{name: "transcript without audio payload bills as text", transcript: "spoken words", bytesPerSecond: 48000, wantText: "spoken words"},
		{name: "g711 audio", audioBytes: 16000, bytesPerSecond: 8000, wantAudio: 27, wantSeconds: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			generation := &realtimeGeneration{info: info, sent: true, promptEstimate: 120, audioBytes: tc.audioBytes, outputBytesPerSecond: tc.bytesPerSecond}
			generation.outputText.WriteString(tc.text)
			generation.transcript.WriteString(tc.transcript)
			estimate := realtimeEstimatedUsage(generation)
			wantText := 0
			if tc.wantText != "" {
				wantText = tokenkit.Estimate("gpt-realtime", tc.wantText)
				require.Positive(t, wantText)
			}
			usage := estimate.usage
			assert.Equal(t, 120, usage.InputTokens)
			assert.Equal(t, 120, usage.InputTokenDetails.TextTokens)
			assert.Equal(t, wantText, usage.OutputTokenDetails.TextTokens)
			assert.Equal(t, tc.wantAudio, usage.OutputTokenDetails.AudioTokens)
			assert.Equal(t, wantText+tc.wantAudio, usage.OutputTokens)
			assert.Equal(t, usage.InputTokens+usage.OutputTokens, usage.TotalTokens)
			assert.Equal(t, tc.audioBytes, estimate.settlement.AudioOutputBytes)
			assert.InDelta(t, tc.wantSeconds, estimate.settlement.AudioOutputSeconds, 1e-9)
		})
	}
}

// Usage rejected by the strict modality checks still proves the generation
// ran; its totals are kept instead of refunding the generation.
func TestRealtimeFallbackUsageKeepsUsableTotals(t *testing.T) {
	for _, tc := range []struct {
		name, usage              string
		input, output            int
		textIn, audioIn, imageIn int
		textOut, audioOut        int
		unusable                 bool
	}{
		{name: "missing modalities", usage: `{"input_tokens":10,"output_tokens":4,"total_tokens":14}`, input: 10, output: 4, textIn: 10, textOut: 4},
		{name: "partial modalities", usage: `{"input_tokens":10,"output_tokens":6,"input_token_details":{"audio_tokens":7},"output_token_details":{"audio_tokens":5}}`, input: 10, output: 6, textIn: 3, audioIn: 7, textOut: 1, audioOut: 5},
		{name: "inconsistent total", usage: `{"input_tokens":10,"output_tokens":6,"total_tokens":99}`, input: 10, output: 6, textIn: 10, textOut: 6},
		{name: "details larger than totals are clamped", usage: `{"input_tokens":10,"output_tokens":2,"input_token_details":{"audio_tokens":50,"image_tokens":3},"output_token_details":{"audio_tokens":9}}`, input: 10, output: 2, audioIn: 10, audioOut: 2},
		{name: "no usable totals", usage: `{"input_tokens":-1,"output_tokens":"x"}`, unusable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Nil(t, ParseRealtimeUsage(gjson.Parse(tc.usage)), "the strict parser rejects this usage")
			usage := realtimeFallbackUsage(gjson.Parse(tc.usage))
			if tc.unusable {
				assert.Nil(t, usage)
				return
			}
			require.NotNil(t, usage)
			assert.Equal(t, tc.input, usage.InputTokens)
			assert.Equal(t, tc.output, usage.OutputTokens)
			assert.Equal(t, tc.input+tc.output, usage.TotalTokens)
			assert.Equal(t, tc.textIn, usage.InputTokenDetails.TextTokens)
			assert.Equal(t, tc.audioIn, usage.InputTokenDetails.AudioTokens)
			assert.Equal(t, tc.imageIn, usage.InputTokenDetails.ImageTokens)
			assert.Equal(t, tc.textOut, usage.OutputTokenDetails.TextTokens)
			assert.Equal(t, tc.audioOut, usage.OutputTokenDetails.AudioTokens)
		})
	}
}

type recordingRealtimeBilling struct {
	refunded, settled bool
}

func (b *recordingRealtimeBilling) Settle(int) error              { b.settled = true; return nil }
func (b *recordingRealtimeBilling) Refund(*gin.Context)           { b.refunded = true }
func (b *recordingRealtimeBilling) NeedsRefund() bool             { return !b.refunded && !b.settled }
func (b *recordingRealtimeBilling) GetPreConsumedQuota() int      { return 0 }
func (b *recordingRealtimeBilling) Reserve(int) error             { return nil }
func (b *recordingRealtimeBilling) ReserveBeforeSubmit(int) error { return nil }

// A failed response.done without usage is an upstream error for that
// generation: it is refunded unless output already reached the client, which
// stays charged.
func TestFinishRealtimeGenerationRefundsFailedResponseWithoutOutput(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Channel{}, &model.Log{}))
	previousDB, previousLogDB, previousRedis := model.DB, model.LOG_DB, common.RedisEnabled
	model.DB, model.LOG_DB, common.RedisEnabled = database, database, false
	t.Cleanup(func() { model.DB, model.LOG_DB, common.RedisEnabled = previousDB, previousLogDB, previousRedis })
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name         string
		status       string
		text         string
		audioBytes   int64
		wantRefunded bool
	}{
		{name: "failed without output is refunded", status: "failed", wantRefunded: true},
		{name: "failed after text delivered is charged", status: "failed", text: "partial answer"},
		{name: "failed after audio delivered is charged", status: "failed", audioBytes: 4800},
		{name: "session ended before response.done is charged", status: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			billing := &recordingRealtimeBilling{}
			info := &relaycommon.RelayInfo{
				ChannelMeta:     &relaycommon.ChannelMeta{UpstreamModelName: "gpt-realtime"},
				OriginModelName: "gpt-realtime",
				RealtimeMetered: true,
				StreamStatus:    relaycommon.NewStreamStatus(),
				Billing:         billing,
				StartTime:       time.Now(),
			}
			generation := &realtimeGeneration{context: c, info: info, sent: true, promptEstimate: 40, audioBytes: tc.audioBytes, outputBytesPerSecond: 48000}
			generation.outputText.WriteString(tc.text)

			finishRealtimeGeneration(generation, nil, tc.status, nil)

			assert.Equal(t, tc.wantRefunded, billing.refunded)
			assert.Equal(t, !tc.wantRefunded, billing.settled)
		})
	}
}

// A transcription turn is charged once its audio reached upstream; an upstream
// error before any transcript was relayed refunds it.
func TestRealtimeTranscriptionMarksSentAfterAudioReachesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		replies  []string
		wantSent bool
	}{
		{name: "interrupted after audio commit is charged", wantSent: true},
		{name: "upstream error before transcript is refunded", replies: []string{`{"type":"error","error":{"message":"rejected"}}`}},
		{
			name: "upstream error after a relayed transcript is charged",
			replies: []string{
				`{"type":"input_audio_buffer.committed","item_id":"up-1"}`,
				`{"type":"conversation.item.input_audio_transcription.delta","item_id":"up-1","delta":"hello"}`,
				`{"type":"error","error":{"message":"late failure"}}`,
			},
			wantSent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					_, raw, err := conn.ReadMessage()
					if err != nil {
						return
					}
					switch gjson.GetBytes(raw, "type").String() {
					case "session.update":
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.updated","session":{"audio":{"input":{"turn_detection":null,"transcription":{"model":"gpt-4o-transcribe"}}}}}`))
					case "input_audio_buffer.commit":
						for _, reply := range tc.replies {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(reply))
						}
						// Without a terminal transcription event the turn is cut off.
						return
					}
				}
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)

			parent, _ := gin.CreateTestContext(httptest.NewRecorder())
			parent.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			connection := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-realtime"},
				RealtimeRequestRunner: func(*http.Request, string, func(*gin.Context) *types.NewAPIError) *types.NewAPIError {
					return nil
				},
			}
			transcriber := newRealtimeTranscriber(1 << 20)
			transcriber.connection = conn
			done := make(chan struct{})
			defer close(done)
			segment := realtimeAudioSegment{
				audio:  make([]byte, 4800),
				itemID: "client-item",
				control: realtimeTurnControl{
					transcription:       "gpt-4o-transcribe",
					transcriptionConfig: gjson.Parse(`{"model":"gpt-4o-transcribe"}`),
					inputBytesPerSecond: 48000,
					inputFormat:         map[string]any{"type": "audio/pcm", "rate": 24000},
				},
			}
			go transcriber.run(parent, connection, "job-1", segment, done)

			var generation *realtimeGeneration
			for frame := range transcriber.events {
				if frame.generation != nil {
					generation = frame.generation
				}
				if frame.err != nil {
					break
				}
			}
			require.NotNil(t, generation)
			assert.Equal(t, tc.wantSent, generation.sent)
		})
	}
}
