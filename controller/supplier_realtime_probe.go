package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// This internal probe needs no consumer API token or wallet. It reserves the
// same procurement budget as real traffic and requires a completed generation.
func testSupplierRealtimeChannel(ctx context.Context, channel *model.Channel, userID int, modelName string) error {
	return testSupplierMeteredRealtimeChannel(ctx, channel, userID, modelName, false)
}
func testSupplierTranscriptionChannel(ctx context.Context, channel *model.Channel, userID int, modelName string) error {
	return testSupplierMeteredRealtimeChannel(ctx, channel, userID, modelName, true)
}
func testSupplierMeteredRealtimeChannel(ctx context.Context, channel *model.Channel, userID int, modelName string, transcription bool) (result error) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/realtime", nil)
	query := c.Request.URL.Query()
	query.Set("model", modelName)
	c.Request.URL.RawQuery = query.Encode()
	cache, err := model.GetUserCache(userID)
	if err != nil {
		return err
	}
	cache.WriteContext(c)
	c.Set("id", userID)
	c.Set(common.RequestIdKey, common.NewRequestId())
	common.SetContextKey(c, constant.ContextKeyUsingGroup, channel.Group)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)
	if apiErr := middleware.SetupContextForSelectedChannel(c, channel, modelName); apiErr != nil {
		return errors.New("Channel test failed")
	}
	info := relaycommon.GenRelayInfoWs(c, nil)
	info.InitChannelMeta(c)
	info.IsChannelTest, info.RealtimeMetered, info.IsStream = true, true, true
	info.RelayMode = relayconstant.RelayModeRealtime
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RequireTerminal()
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return err
	}
	if err := service.PrepareChannelEventAttempt(info, channel); err != nil {
		return err
	}
	if _, err := helper.ModelPriceHelper(c, info, 16, &types.TokenCountMeta{MaxTokens: 16}); err != nil {
		return err
	}
	defer func() {
		var failure *types.NewAPIError
		if result != nil {
			failure = types.NewError(result, types.ErrorCodeDoRequestFailed)
		}
		if err := service.FinishChannelLimitAttempt(c, info, failure); err != nil && result == nil {
			result = err
		}
	}()
	if transcription {
		c.Set("realtime_transcription_session", true)
	}
	adaptor := relay.GetAdaptor(info.ApiType)
	if adaptor == nil {
		return errors.New("Channel test failed")
	}
	adaptor.Init(info)
	conn, err := relaychannel.DoWssRequest(adaptor, c, info, nil)
	if err != nil {
		return errors.New("Channel test failed")
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Minute)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_, created, err := conn.ReadMessage()
	if err != nil {
		return errors.New("Channel test failed")
	}
	legacyTranscription := gjson.GetBytes(created, "type").String() == "transcription_session.created"
	if gjson.GetBytes(created, "type").String() != "session.created" && !(transcription && legacyTranscription) {
		return errors.New("Channel test failed")
	}
	modern := gjson.GetBytes(created, "session.audio").Exists()
	session := map[string]any{"turn_detection": nil, "input_audio_transcription": nil}
	if modern {
		session = map[string]any{"type": "realtime", "audio": map[string]any{"input": map[string]any{"turn_detection": nil, "transcription": nil}}}
	}
	updateType := "session.update"
	if transcription {
		modern = !legacyTranscription
		session = map[string]any{"type": "transcription", "audio": map[string]any{"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "turn_detection": nil, "transcription": map[string]any{"model": info.UpstreamModelName}}}}
		if legacyTranscription {
			updateType = "transcription_session.update"
			session = map[string]any{"input_audio_format": "pcm16", "turn_detection": nil, "input_audio_transcription": map[string]any{"model": info.UpstreamModelName}}
		}
	}
	update, _ := common.Marshal(map[string]any{"type": updateType, "session": session})
	if err := conn.WriteMessage(websocket.TextMessage, update); err != nil {
		return errors.New("Channel test failed")
	}
	for {
		_, event, err := conn.ReadMessage()
		if err != nil {
			return errors.New("Channel test failed")
		}
		kind := gjson.GetBytes(event, "type").String()
		if kind == "session.updated" || kind == "transcription_session.updated" {
			if transcription {
				vad, asrModel := "session.audio.input.turn_detection", "session.audio.input.transcription.model"
				if legacyTranscription {
					vad, asrModel = "session.turn_detection", "session.input_audio_transcription.model"
				}
				if value := gjson.GetBytes(event, vad); !value.Exists() || value.Type != gjson.Null || gjson.GetBytes(event, asrModel).String() != info.UpstreamModelName {
					return errors.New("Channel test failed")
				}
			} else if !openai.ControlledRealtimeSessionConfirmed(gjson.GetBytes(event, "session")) {
				return errors.New("Channel test failed")
			}
			break
		}
		if kind == "error" {
			return errors.New("Channel test failed")
		}
	}
	response := map[string]any{"max_output_tokens": 16, "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply OK."}}}}, "conversation": "none"}
	if modern {
		response["output_modalities"] = []string{"text"}
	} else {
		response["modalities"] = []string{"text"}
	}
	payload, _ := common.Marshal(map[string]any{"type": "response.create", "response": response})
	prompt, output, total := service.RealtimeGenerationBounds(info, gjson.Parse("16"))
	if transcription {
		prompt, output, total = service.RealtimeGenerationBounds(info, gjson.Result{})
		if strings.HasPrefix(info.UpstreamModelName, "whisper-") {
			quantity, clamp := common.QuotaRoundChecked(math.Ceil(0.5) / 60 * 1000)
			if clamp != nil {
				return clamp
			}
			input, zero := int64(quantity), int64(0)
			prompt, output, total = &input, &zero, nil
			info.ChannelLimitAttempt.TPMApplicable = false
		}
		payload, _ = common.Marshal(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(make([]byte, 24000))})
	}
	if err := service.AdmitChannelEvent(c, info, prompt, output, total); err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return errors.New("Channel test failed")
	}
	completedItemID, responseID := "", ""
	if transcription {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
			return errors.New("Channel test failed")
		}
	}
	for {
		_, event, err := conn.ReadMessage()
		if err != nil {
			return errors.New("Channel test failed")
		}
		switch gjson.GetBytes(event, "type").String() {
		case "error":
			return errors.New("Channel test failed")
		case "input_audio_buffer.committed":
			completedItemID = gjson.GetBytes(event, "item_id").String()
		case "response.created":
			responseID = gjson.GetBytes(event, "response.id").String()
		case "conversation.item.input_audio_transcription.failed":
			return errors.New("Channel test failed")
		case "conversation.item.input_audio_transcription.completed":
			if !transcription || completedItemID == "" || gjson.GetBytes(event, "item_id").String() != completedItemID {
				return errors.New("Channel test failed")
			}
			info.StreamStatus.MarkCompleted()
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			raw := gjson.GetBytes(event, "usage")
			if usage := openai.ParseRealtimeUsage(raw); usage != nil {
				service.CaptureRealtimeChannelTestUsage(c, info, usage)
			}
			if raw.Get("type").String() == "duration" {
				seconds := raw.Get("seconds")
				if seconds.Type == gjson.Number && seconds.Num >= 0 && seconds.Num <= relaycommon.MaxTaskDurationSeconds {
					service.CaptureRealtimeTranscriptionTestDuration(c, info, seconds.Num)
				}
			}
			return nil
		case "response.done":
			if transcription || responseID == "" || gjson.GetBytes(event, "response.id").String() != responseID || gjson.GetBytes(event, "response.status").String() != "completed" {
				return errors.New("Channel test failed")
			}
			info.StreamStatus.MarkCompleted()
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			if usage := openai.ParseRealtimeUsage(gjson.GetBytes(event, "response.usage")); usage != nil {
				service.CaptureRealtimeChannelTestUsage(c, info, usage)
			}
			return nil
		}
	}
}
