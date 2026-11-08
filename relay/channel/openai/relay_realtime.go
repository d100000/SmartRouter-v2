package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type realtimeFrame struct {
	body []byte
	err  error
}
type realtimeGeneration struct {
	context                            *gin.Context
	info                               *relaycommon.RelayInfo
	eventID, clientEventID, responseID string
	audioBytes                         int64
	billed                             bool
}
type realtimeTurnControl struct {
	enabled, create, interrupt bool
	idle                       time.Duration
	output                     gjson.Result
	turnDetection              gjson.Result
	transcription              string
	transcriptionConfig        gjson.Result
	inputBytesPerSecond        int64
	inputFormat                any
	legacyInputFormat          string
	noiseReduction             gjson.Result
	include                    gjson.Result
	prefixPaddingMS            int64
	outputBytesPerSecond       int64
}

// One goroutine owns protocol state, reservations and settlement. Socket
// readers only enqueue immutable frames; they never mutate billing objects.
func OpenaiRealtimeHandler(c *gin.Context, info *relaycommon.RelayInfo) (*types.NewAPIError, *dto.RealtimeUsage) {
	if info == nil || info.ClientWs == nil || info.TargetWs == nil || info.RealtimeRequestRunner == nil {
		return types.NewError(errors.New("invalid metered realtime connection"), types.ErrorCodeBadResponse), nil
	}
	info.RealtimeMetered = true
	info.IsStream = true
	client, target := info.ClientWs, info.TargetWs
	maxFrame := int64(constant.MaxRequestBodyMB) << 20
	if maxFrame <= 0 {
		maxFrame = 128 << 20
	}
	client.SetReadLimit(maxFrame)
	target.SetReadLimit(maxFrame)
	done := make(chan struct{})
	sessionContext, cancelSession := context.WithCancel(c.Request.Context())
	c.Request = c.Request.Clone(sessionContext)
	defer target.Close()
	clientFrames, targetFrames := make(chan realtimeFrame, 4), make(chan realtimeFrame, 4)
	go readRealtimeFrames(client, clientFrames, done)
	go readRealtimeFrames(target, targetFrames, done)
	pending := []*realtimeGeneration{}
	active := map[string]*realtimeGeneration{}
	controls := realtimeTurnControl{enabled: true, create: true, interrupt: true, outputBytesPerSecond: 48000, inputBytesPerSecond: 48000, inputFormat: map[string]any{"type": "audio/pcm", "rate": 24000}, legacyInputFormat: "pcm16"}
	ready := false
	var pendingControl *realtimeTurnControl
	pendingControlID := ""
	var pendingControlSince time.Time
	pendingClearID, pendingClearClientID := "", ""
	var pendingClearSince time.Time
	seenCompletions := map[string]bool{}
	completionOrder := []string{}
	watchdog := time.NewTicker(time.Second)
	defer watchdog.Stop()
	responseTimeout := time.Duration(constant.StreamingTimeout) * time.Second
	if responseTimeout <= 0 {
		responseTimeout = 300 * time.Second
	}
	var savedCreated []byte
	transcriber := newRealtimeTranscriber(maxFrame)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var idle <-chan time.Time
	defer func() {
		failure := types.NewError(context.Canceled, types.ErrorCodeDoRequestFailed)
		for _, generation := range pending {
			finishRealtimeGeneration(generation, nil, "", failure)
		}
		for _, generation := range active {
			finishRealtimeGeneration(generation, nil, "", failure)
		}
		cancelSession()
		close(done)
		transcriber.workers.Wait()
		transcriber.all.Range(func(_, value any) bool {
			finishRealtimeGeneration(value.(*realtimeGeneration), nil, "", failure)
			return true
		})
	}()
	for {
		var incoming <-chan realtimeFrame
		if ready && pendingControl == nil && pendingClearID == "" {
			incoming = clientFrames
		}
		select {
		case <-c.Request.Context().Done():
			return nil, &dto.RealtimeUsage{}
		case now := <-watchdog.C:
			if !ready && now.Sub(info.StartTime) > responseTimeout {
				return realtimeProtocolError("upstream session setup timed out"), nil
			}
			if pendingClearID != "" && now.Sub(pendingClearSince) > responseTimeout {
				return realtimeProtocolError("upstream audio clear timed out"), nil
			}
			if pendingControl != nil && now.Sub(pendingControlSince) > responseTimeout {
				return realtimeProtocolError("upstream session update timed out"), nil
			}
			for _, generation := range pending {
				if now.Sub(generation.info.StartTime) > responseTimeout {
					return nil, &dto.RealtimeUsage{}
				}
			}
			for _, generation := range active {
				if now.Sub(generation.info.StartTime) > responseTimeout {
					return nil, &dto.RealtimeUsage{}
				}
			}

		case <-idle:
			idle = nil
			generation, payload, apiErr := prepareRealtimeGeneration(c, info, []byte(`{"type":"response.create"}`), controls.output, info.OriginModelName, "realtime_generation")
			if apiErr != nil {
				writeRealtimeError(client, "", apiErr)
				continue
			}
			if err := writeRealtimeFrame(target, payload); err != nil {
				finishRealtimeGeneration(generation, nil, "", types.NewError(err, types.ErrorCodeDoRequestFailed))
				return nil, &dto.RealtimeUsage{}
			}
			pending = append(pending, generation)
		case frame := <-incoming:
			if frame.err != nil {
				return nil, &dto.RealtimeUsage{}
			}
			if !gjson.ValidBytes(frame.body) {
				writeRealtimeError(client, "", realtimeProtocolError("invalid realtime event"))
				continue
			}
			event := gjson.ParseBytes(frame.body)
			eventID := event.Get("event_id").String()
			switch event.Get("type").String() {
			case "session.update":
				next, err := realtimeControls(event.Get("session"), controls)
				if err != nil {
					writeRealtimeError(client, eventID, realtimeProtocolError(err.Error()))
					continue
				}
				frame.body, err = controlledRealtimeSession(frame.body, next)
				if err != nil {
					writeRealtimeError(client, eventID, realtimeProtocolError("invalid session update"))
					continue
				}
				pendingControl = &next
				pendingControlID = eventID
				pendingControlSince = time.Now()
				idle = nil
				timer.Stop()
			case "response.create":
				if len(pending)+len(active) >= 16 {
					writeRealtimeError(client, eventID, realtimeProtocolError("too many pending responses"))
					continue
				}
				output := event.Get("response.max_output_tokens")
				if !output.Exists() {
					output = controls.output
				}
				generation, payload, apiErr := prepareRealtimeGeneration(c, info, frame.body, output, info.OriginModelName, "realtime_generation")
				if apiErr != nil {
					writeRealtimeError(client, eventID, apiErr)
					continue
				}
				if err := writeRealtimeFrame(target, payload); err != nil {
					finishRealtimeGeneration(generation, nil, "", types.NewError(err, types.ErrorCodeDoRequestFailed))
					return nil, &dto.RealtimeUsage{}
				}
				pending = append(pending, generation)
				idle = nil
				timer.Stop()
				continue
			case "input_audio_buffer.append":
				if err := transcriber.append(event, controls); err != nil {
					writeRealtimeError(client, eventID, realtimeProtocolError(err.Error()))
					continue
				}
			case "conversation.item.create":
				if event.Get("item.id").String() == "" {
					frame.body, _ = sjson.SetBytes(frame.body, "item.id", common.NewRequestId())
					event = gjson.ParseBytes(frame.body)
				}
				frame.body, _ = sjson.SetBytes(frame.body, "event_id", common.NewRequestId())
				event = gjson.ParseBytes(frame.body)
				if err := transcriber.item(event, controls, eventID); err != nil {
					writeRealtimeError(client, eventID, realtimeProtocolError(err.Error()))
					continue
				}
			case "input_audio_buffer.commit":
				internalID := common.NewRequestId()
				if err := transcriber.commit(controls, internalID, eventID); err != nil {
					writeRealtimeError(client, eventID, realtimeProtocolError(err.Error()))
					continue
				}
				frame.body, _ = sjson.SetBytes(frame.body, "event_id", internalID)
			case "input_audio_buffer.clear":
				pendingClearID, pendingClearClientID = common.NewRequestId(), eventID
				pendingClearSince = time.Now()
				frame.body, _ = sjson.SetBytes(frame.body, "event_id", pendingClearID)
			}
			if err := writeRealtimeFrame(target, frame.body); err != nil {
				return nil, &dto.RealtimeUsage{}
			}
		case frame := <-transcriber.events:
			if frame.generation != nil {
				transcriber.jobs[frame.jobID] = frame.generation
			}
			if frame.err != nil {
				finishRealtimeGeneration(transcriber.jobs[frame.jobID], nil, "", types.NewError(frame.err, types.ErrorCodeDoRequestFailed))
				delete(transcriber.jobs, frame.jobID)
				transcriber.all.Delete(frame.jobID)
				writeRealtimeError(client, "", realtimeProtocolError("transcription request failed"))
				continue
			}
			if len(frame.body) == 0 {
				continue
			}
			event := gjson.ParseBytes(frame.body)
			kind := event.Get("type").String()
			if strings.HasSuffix(kind, ".completed") || strings.HasSuffix(kind, ".failed") {
				status := "completed"
				if strings.HasSuffix(kind, ".failed") {
					status = "failed"
				}
				usage := ParseRealtimeUsage(event.Get("usage"))
				generation := transcriber.jobs[frame.jobID]
				if generation != nil && event.Get("usage.type").String() == "duration" {
					seconds := event.Get("usage.seconds")
					if seconds.Type == gjson.Number && seconds.Num >= 0 && seconds.Num <= relaycommon.MaxTaskDurationSeconds {
						generation.billed = service.SettleRealtimeTranscriptionDuration(generation.context, generation.info, seconds.Num)
					}
				}
				finishRealtimeGeneration(generation, usage, status, nil)
				delete(transcriber.jobs, frame.jobID)
				transcriber.all.Delete(frame.jobID)
			}
			if err := writeRealtimeFrame(client, service.RedactUpstreamWebSocketCredential(frame.body, info.ApiKey)); err != nil {
				return nil, &dto.RealtimeUsage{}
			}
		case frame := <-targetFrames:
			if frame.err != nil {
				return nil, &dto.RealtimeUsage{}
			}
			if !gjson.ValidBytes(frame.body) {
				return types.NewError(errors.New("invalid upstream realtime event"), types.ErrorCodeBadResponse), nil
			}
			event := gjson.ParseBytes(frame.body)
			eventType := event.Get("type").String()
			switch eventType {
			case "session.created":
				var err error
				controls, err = realtimeControls(event.Get("session"), controls)
				if err != nil {
					return realtimeProtocolError("invalid upstream session configuration"), nil
				}
				savedCreated = frame.body
				update := []byte(`{"type":"session.update","session":{}}`)
				if event.Get("session.audio").Exists() {
					update = []byte(`{"type":"session.update","session":{"type":"realtime","audio":{"input":{}}}}`)
				}
				update, err = controlledRealtimeSession(update, controls)
				if err != nil {
					return realtimeProtocolError("invalid upstream session"), nil
				}
				if err = writeRealtimeFrame(target, update); err != nil {
					return types.NewError(err, types.ErrorCodeDoRequestFailed), nil
				}
				continue
			case "session.updated":
				if !ControlledRealtimeSessionConfirmed(event.Get("session")) {
					return realtimeProtocolError("upstream did not confirm metered session controls"), nil
				}
				if !ready {
					ready = true
					if err := writeRealtimeFrame(client, service.RedactUpstreamWebSocketCredential(savedCreated, info.ApiKey)); err != nil {
						return nil, &dto.RealtimeUsage{}
					}
					continue
				}
				if pendingControl != nil {
					controls = *pendingControl
					pendingControl = nil
					pendingControlID = ""
				}
				// Report the requested VAD semantics, which the gateway now implements.
				frame.body, _ = restoreRealtimeControlView(frame.body, controls)
			case "input_audio_buffer.cleared":
				transcriber.clear()
				pendingClearID, pendingClearClientID = "", ""
			case "input_audio_buffer.speech_started":
				idle = nil
				timer.Stop()
				if controls.interrupt {
					for id := range active {
						cancel, _ := common.Marshal(map[string]any{"type": "response.cancel", "response_id": id})
						if writeRealtimeFrame(target, cancel) != nil {
							return nil, &dto.RealtimeUsage{}
						}
					}
				}
			case "input_audio_buffer.committed":
				manual := transcriber.isManualCommit(event)
				if controls.enabled && controls.create && !manual && len(pending)+len(active) >= 16 {
					writeRealtimeError(client, "", realtimeProtocolError("too many pending responses"))
				} else if controls.enabled && controls.create && !manual {
					generation, payload, apiErr := prepareRealtimeGeneration(c, info, []byte(`{"type":"response.create"}`), controls.output, info.OriginModelName, "realtime_generation")
					if apiErr != nil {
						writeRealtimeError(client, "", apiErr)
					} else if err := writeRealtimeFrame(target, payload); err != nil {
						finishRealtimeGeneration(generation, nil, "", types.NewError(err, types.ErrorCodeDoRequestFailed))
						return nil, &dto.RealtimeUsage{}
					} else {
						pending = append(pending, generation)
					}
				}
			case "response.created":
				id := event.Get("response.id").String()
				if active[id] != nil {
					continue
				}
				if id == "" || len(pending) == 0 {
					return types.NewError(errors.New("upstream generated a response without admission"), types.ErrorCodeBadResponse), nil
				}
				active[id] = pending[0]
				active[id].responseID = id
				pending = pending[1:]
			case "response.audio.delta", "response.output_audio.delta":
				if generation := active[event.Get("response_id").String()]; generation != nil {
					if data, err := base64.StdEncoding.DecodeString(event.Get("delta").String()); err == nil {
						generation.audioBytes += int64(len(data))
					}
				}
			case "response.done":
				id := event.Get("response.id").String()
				if seenCompletions[id] {
					continue
				}
				generation := active[id]
				if generation == nil {
					return types.NewError(errors.New("unmatched realtime completion"), types.ErrorCodeBadResponse), nil
				}
				delete(active, id)
				seenCompletions[id] = true
				completionOrder = append(completionOrder, id)
				if len(completionOrder) > 128 {
					delete(seenCompletions, completionOrder[0])
					completionOrder = completionOrder[1:]
				}
				usage := ParseRealtimeUsage(event.Get("response.usage"))
				finishRealtimeGeneration(generation, usage, event.Get("response.status").String(), nil)
				if controls.enabled && controls.create && controls.idle > 0 && len(active) == 0 && len(pending) == 0 {
					playback := time.Duration(0)
					if controls.outputBytesPerSecond > 0 {
						playback = time.Duration(generation.audioBytes) * time.Second / time.Duration(controls.outputBytesPerSecond)
					}
					timer.Reset(controls.idle + playback)
					idle = timer.C
				}
			case "conversation.item.input_audio_transcription.completed", "conversation.item.input_audio_transcription.failed":
				return types.NewError(errors.New("unadmitted upstream transcription"), types.ErrorCodeBadResponse), nil
			case "error":
				if !ready {
					return realtimeProtocolError("upstream cannot disable automatic response generation"), nil
				}
				failedID := event.Get("error.event_id").String()
				if failedID == pendingClearID && pendingClearID != "" {
					frame.body, _ = sjson.SetBytes(frame.body, "error.event_id", pendingClearClientID)
					pendingClearID, pendingClearClientID = "", ""
				}
				if clientID, found := transcriber.rejectItem(failedID); found {
					frame.body, _ = sjson.SetBytes(frame.body, "error.event_id", clientID)
				}
				if clientID, found := transcriber.rejectCommit(failedID); found {
					frame.body, _ = sjson.SetBytes(frame.body, "error.event_id", clientID)
				}
				if pendingControl != nil && (failedID == pendingControlID || failedID == "") {
					pendingControl = nil
					pendingControlID = ""
				}
				for index, generation := range pending {
					if generation.eventID != failedID {
						continue
					}
					pending = append(pending[:index], pending[index+1:]...)
					finishRealtimeGeneration(generation, nil, "failed", nil)
					frame.body, _ = sjson.SetBytes(frame.body, "error.event_id", generation.clientEventID)
					break
				}
			}
			if err := transcriber.observe(c, info, event, controls, done); err != nil {
				writeRealtimeError(client, "", realtimeProtocolError(err.Error()))
			}
			if id := event.Get("response_id").String(); id != "" {
				if generation := active[id]; generation != nil {
					generation.info.SetFirstResponseTime()
					service.ObserveSchedulingOutput(generation.context, frame.body)
				}
			}
			if err := writeRealtimeFrame(client, service.RedactUpstreamWebSocketCredential(frame.body, info.ApiKey)); err != nil {
				return nil, &dto.RealtimeUsage{}
			}
		}
	}
}

func readRealtimeFrames(conn *websocket.Conn, frames chan<- realtimeFrame, done <-chan struct{}) {
	for {
		kind, body, err := conn.ReadMessage()
		if err == nil && kind != websocket.TextMessage {
			err = errors.New("realtime requires JSON text events")
		}
		select {
		case frames <- realtimeFrame{body, err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}
func writeRealtimeFrame(conn *websocket.Conn, body []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, body)
}
func realtimeProtocolError(message string) *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
}
func writeRealtimeError(conn *websocket.Conn, eventID string, apiErr *types.NewAPIError) {
	safe := apiErr.ToOpenAIError()
	payload, _ := common.Marshal(map[string]any{"type": "error", "event_id": common.NewRequestId(), "error": map[string]any{"type": safe.Type, "code": safe.Code, "message": safe.Message, "event_id": eventID}})
	_ = writeRealtimeFrame(conn, payload)
}

func validRealtimeOutputLimit(value gjson.Result) bool {
	if value.Type != gjson.Number || value.Num < 0 || math.Trunc(value.Num) != value.Num || value.Num > float64(common.MaxQuota) {
		return false
	}
	limit := uint(value.Int())
	return !helper.ExceedsMaxTokensLimit(&limit)
}

func realtimeControls(session gjson.Result, previous realtimeTurnControl) (realtimeTurnControl, error) {
	control := previous
	if !session.IsObject() {
		return control, errors.New("session must be an object")
	}
	vad := session.Get("turn_detection")
	if value := session.Get("audio.input.turn_detection"); value.Exists() {
		vad = value
	}
	if vad.Exists() {
		control.turnDetection = vad
		control.enabled = vad.Type != gjson.Null
		if control.enabled {
			if !vad.IsObject() {
				return control, errors.New("turn_detection must be an object or null")
			}
			control.create, control.interrupt = true, true
			if value := vad.Get("create_response"); value.Exists() {
				if value.Type != gjson.True && value.Type != gjson.False {
					return control, errors.New("create_response must be boolean")
				}
				control.create = value.Bool()
			}
			if value := vad.Get("interrupt_response"); value.Exists() {
				if value.Type != gjson.True && value.Type != gjson.False {
					return control, errors.New("interrupt_response must be boolean")
				}
				control.interrupt = value.Bool()
			}
			if vad.Get("type").String() == "semantic_vad" {
				control.idle = 0
				if value := vad.Get("idle_timeout_ms"); value.Exists() && value.Type != gjson.Null {
					return control, errors.New("idle_timeout_ms requires server_vad")
				}
			}
			if value := vad.Get("idle_timeout_ms"); value.Exists() {
				if value.Type == gjson.Null {
					control.idle = 0
				} else if value.Type == gjson.Number && value.Num >= 5000 && value.Num <= 30000 && math.Trunc(value.Num) == value.Num {
					control.idle = time.Duration(value.Int()) * time.Millisecond
				} else {
					return control, errors.New("invalid idle_timeout_ms")
				}
			}
		} else {
			control.idle = 0
		}
	}
	for _, path := range []string{"max_response_output_tokens", "max_output_tokens"} {
		if value := session.Get(path); value.Exists() {
			control.output = value
		}
	}
	noise := session.Get("input_audio_noise_reduction")
	if value := session.Get("audio.input.noise_reduction"); value.Exists() {
		noise = value
	}
	if noise.Exists() {
		control.noiseReduction = noise
	}
	if include := session.Get("include"); include.Exists() {
		control.include = include
	}
	transcription := session.Get("input_audio_transcription")
	if value := session.Get("audio.input.transcription"); value.Exists() {
		transcription = value
	}
	if transcription.Exists() {
		control.transcription = transcription.Get("model").String()
		control.transcriptionConfig = transcription
	}
	if value := vad.Get("prefix_padding_ms"); value.Type == gjson.Number && value.Num >= 0 && value.Num <= 3600000 {
		control.prefixPaddingMS = value.Int()
	}
	inputFormat := session.Get("input_audio_format")
	if value := session.Get("audio.input.format"); value.IsObject() {
		inputFormat = value.Get("type")
	}
	if inputFormat.Exists() {
		switch inputFormat.String() {
		case "pcm16", "audio/pcm":
			control.inputBytesPerSecond = 48000
			control.legacyInputFormat = "pcm16"
			control.inputFormat = map[string]any{"type": "audio/pcm", "rate": 24000}
		case "g711_ulaw", "audio/pcmu":
			control.inputBytesPerSecond = 8000
			control.legacyInputFormat = "g711_ulaw"
			control.inputFormat = map[string]any{"type": "audio/pcmu"}
		case "g711_alaw", "audio/pcma":
			control.inputBytesPerSecond = 8000
			control.legacyInputFormat = "g711_alaw"
			control.inputFormat = map[string]any{"type": "audio/pcma"}
		default:
			return control, errors.New("unsupported input audio format")
		}
	}
	format := session.Get("output_audio_format").String()
	if value := session.Get("audio.output.format.type"); value.Exists() {
		format = value.String()
	}
	switch format {
	case "":
	case "pcm16", "audio/pcm":
		control.outputBytesPerSecond = 48000
	case "g711_ulaw", "g711_alaw", "audio/pcmu", "audio/pcma":
		control.outputBytesPerSecond = 8000
	default:
		control.outputBytesPerSecond = 0
	}
	if control.output.Exists() && control.output.String() != "inf" {
		if !validRealtimeOutputLimit(control.output) {
			return control, errors.New("invalid max_output_tokens")
		}
	}
	return control, nil
}
func controlledRealtimeSession(payload []byte, control realtimeTurnControl) ([]byte, error) {
	path := "session.turn_detection"
	if gjson.GetBytes(payload, "session.audio").Exists() {
		path = "session.audio.input.turn_detection"
	}
	transcriptionPath := "session.input_audio_transcription"
	if strings.Contains(path, "audio.input") {
		transcriptionPath = "session.audio.input.transcription"
	}
	payload, _ = sjson.SetBytes(payload, transcriptionPath, nil)
	if !control.enabled {
		return sjson.SetBytes(payload, path, nil)
	}
	if !gjson.GetBytes(payload, path).IsObject() {
		if control.turnDetection.IsObject() {
			payload, _ = sjson.SetRawBytes(payload, path, []byte(control.turnDetection.Raw))
		} else {
			payload, _ = sjson.SetBytes(payload, path, map[string]any{"type": "server_vad"})
		}
	}
	var err error
	for _, field := range []string{"create_response", "interrupt_response"} {
		payload, err = sjson.SetBytes(payload, path+"."+field, false)
		if err != nil {
			return nil, err
		}
	}
	if gjson.GetBytes(payload, path+".type").String() == "semantic_vad" {
		return sjson.DeleteBytes(payload, path+".idle_timeout_ms")
	}
	return sjson.SetBytes(payload, path+".idle_timeout_ms", nil)
}
func ControlledRealtimeSessionConfirmed(session gjson.Result) bool {
	vad, transcription := session.Get("turn_detection"), session.Get("input_audio_transcription")
	if session.Get("audio").Exists() {
		vad, transcription = session.Get("audio.input.turn_detection"), session.Get("audio.input.transcription")
	}
	if !vad.Exists() || !transcription.Exists() || transcription.Type != gjson.Null {
		return false
	}
	if vad.Type == gjson.Null {
		return true
	}
	return vad.IsObject() && vad.Get("create_response").Type == gjson.False && vad.Get("interrupt_response").Type == gjson.False && (!vad.Get("idle_timeout_ms").Exists() || vad.Get("idle_timeout_ms").Type == gjson.Null)
}
func restoreRealtimeControlView(payload []byte, control realtimeTurnControl) ([]byte, error) {
	path := "session.turn_detection"
	if gjson.GetBytes(payload, "session.audio").Exists() {
		path = "session.audio.input.turn_detection"
	}
	if control.transcriptionConfig.Exists() {
		transcriptionPath := "session.input_audio_transcription"
		if strings.Contains(path, "audio.input") {
			transcriptionPath = "session.audio.input.transcription"
		}
		payload, _ = sjson.SetRawBytes(payload, transcriptionPath, []byte(control.transcriptionConfig.Raw))
	}
	if !control.enabled {
		return sjson.SetBytes(payload, path, nil)
	}
	payload, _ = sjson.SetBytes(payload, path+".create_response", control.create)
	payload, _ = sjson.SetBytes(payload, path+".interrupt_response", control.interrupt)
	if control.idle > 0 {
		return sjson.SetBytes(payload, path+".idle_timeout_ms", control.idle.Milliseconds())
	}
	return payload, nil
}

func prepareRealtimeGeneration(parent *gin.Context, connection *relaycommon.RelayInfo, payload []byte, output gjson.Result, modelName, stage string, audioSeconds ...float64) (*realtimeGeneration, []byte, *types.NewAPIError) {
	generation := &realtimeGeneration{eventID: common.NewRequestId(), clientEventID: gjson.GetBytes(payload, "event_id").String()}
	if output.Exists() && output.String() != "inf" && (!validRealtimeOutputLimit(output)) {
		return nil, nil, realtimeProtocolError("invalid max_output_tokens")
	}
	authBody, _ := common.Marshal(map[string]any{"model": modelName})
	request := parent.Request.Clone(parent.Request.Context())
	request.Body = http.NoBody
	request.ContentLength = int64(len(authBody))
	request.Body = io.NopCloser(bytes.NewReader(authBody))
	apiErr := connection.RealtimeRequestRunner(request, generation.eventID, func(c *gin.Context) (failure *types.NewAPIError) {
		defer func() {
			if failure != nil && generation.info != nil {
				if generation.info.Billing != nil {
					generation.info.Billing.Refund(c)
				}
				service.EndSchedulingRequest(c)
			}
		}()
		if common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
			allowed, _ := common.GetContextKeyType[map[string]bool](c, constant.ContextKeyTokenModelLimit)
			if !allowed[modelName] && !allowed[ratio_setting.FormatMatchingModelName(modelName)] && !allowed[ratio_setting.RoutingMatchModelName(modelName)] {
				return types.NewErrorWithStatusCode(errors.New("token model is not authorized"), types.ErrorCodeAccessDenied, http.StatusForbidden)
			}
		}
		channel, err := model.GetChannelById(connection.ChannelId, true)
		if err != nil {
			return service.ChannelLimitRelayError(err, c)
		}
		if connection.ChannelLimitAttempt == nil || channel.Type != connection.ChannelType || !reflect.DeepEqual(channel.GetHeaderOverride(), connection.HeadersOverride) || channel.GetSetting().Proxy != connection.ChannelSetting.Proxy || channel.GetBaseURL() != common.GetContextKeyString(parent, constant.ContextKeyChannelBaseUrl) || channel.OwnerSupplierID != connection.ChannelLimitAttempt.SupplierID {
			return realtimeProtocolError("upstream connection changed; reconnect required")
		}
		keys := channel.GetKeys()
		index := connection.ChannelMultiKeyIndex
		if !channel.ChannelInfo.IsMultiKey {
			keys = []string{channel.Key}
			index = 0
		}
		if index < 0 || index >= len(keys) || keys[index] != connection.ApiKey {
			return realtimeProtocolError("upstream credential changed; reconnect required")
		}
		if status := channel.ChannelInfo.MultiKeyStatusList[index]; status != 0 && status != common.ChannelStatusEnabled {
			return realtimeProtocolError("upstream credential is disabled")
		}
		group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		if group == "auto" {
			if !slices.Contains(service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup)), connection.UsingGroup) {
				return realtimeProtocolError("connection group is no longer authorized")
			}
			group = connection.UsingGroup
			common.SetContextKey(c, constant.ContextKeyAutoGroup, group)
		}
		if !model.IsChannelEnabledForGroupModel(group, modelName, channel.Id) {
			return realtimeProtocolError("channel model is no longer authorized")
		}
		for _, key := range []constant.ContextKey{constant.ContextKeyChannelId, constant.ContextKeyChannelName, constant.ContextKeyChannelType, constant.ContextKeyChannelCreateTime, constant.ContextKeyChannelSetting, constant.ContextKeyChannelOtherSetting, constant.ContextKeyChannelParamOverride, constant.ContextKeyChannelHeaderOverride, constant.ContextKeyChannelOrganization, constant.ContextKeyChannelAutoBan, constant.ContextKeyChannelModelMapping, constant.ContextKeyChannelStatusCodeMapping, constant.ContextKeyChannelIsMultiKey, constant.ContextKeyChannelMultiKeyIndex, constant.ContextKeyChannelKey, constant.ContextKeyChannelBaseUrl, constant.ContextKeyUpstreamRequestOrigin} {
			if value, ok := parent.Get(string(key)); ok {
				c.Set(string(key), value)
			}
		}
		common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)
		c.Request.URL.Path = "/v1/realtime"
		info := relaycommon.GenRelayInfoWs(c, connection.ClientWs)
		info.InitChannelMeta(c)
		info.RealtimeMetered, info.IsStream = true, true
		info.RelayMode = relayconstant.RelayModeRealtime
		info.StreamStatus = relaycommon.NewStreamStatus()
		info.StreamStatus.RequireTerminal()
		info.BillingRequestInput = &billingexpr.RequestInput{Body: append([]byte(nil), payload...), Headers: map[string]string{}}
		for key, values := range c.Request.Header {
			if len(values) > 0 {
				info.BillingRequestInput.Headers[key] = values[0]
			}
		}
		if err := helper.ModelMappedHelper(c, info, nil); err != nil {
			return realtimeProtocolError("invalid model mapping")
		}
		generation.info = info
		if err := service.PrepareChannelEventAttempt(info, channel); err != nil {
			return service.ChannelLimitRelayError(err, c)
		}
		prompt, completion, total := service.RealtimeGenerationBounds(info, output)
		if stage == "realtime_transcription" {
			if strings.HasPrefix(info.UpstreamModelName, "whisper-") && len(audioSeconds) > 0 {
				info.ChannelLimitAttempt.TPMApplicable = false
				total = nil
				quantity, clamp := common.QuotaRoundChecked(math.Ceil(audioSeconds[0]) / 60 * 1000)
				if clamp != nil {
					return types.NewError(clamp, types.ErrorCodeModelPriceError)
				}
				value, zero := int64(quantity), int64(0)
				prompt, completion = &value, &zero
			}
		}
		estimated := 0
		if prompt != nil {
			estimated = int(*prompt)
		}
		price, err := helper.ModelPriceHelper(c, info, estimated, &types.TokenCountMeta{})
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeModelPriceError, http.StatusBadRequest)
		}
		if !price.FreeModel {
			if apiErr := service.PreConsumeBilling(c, price.QuotaToPreConsume, info); apiErr != nil {
				return apiErr
			}
		}
		if err := service.ReserveSchedulingChannel(c, channel, group, modelName); err != nil {
			return service.ChannelLimitRelayError(err, c)
		}
		service.RequestPolicy(c).BeginAttempt(channel, group)
		service.PrepareOverviewAttempt(c, info, channel)
		if info.OverviewAttempt != nil {
			info.OverviewAttempt.CallStage = stage
		}
		if err := service.AdmitChannelEvent(c, info, prompt, completion, total); err != nil {
			return service.ChannelLimitRelayError(err, c)
		}
		service.MarkOverviewUpstreamSent(info)
		generation.context = c.Copy()
		return nil
	})
	if apiErr != nil {
		return nil, nil, apiErr
	}
	outgoing, err := sjson.SetBytes(payload, "event_id", generation.eventID)
	if err != nil {
		finishRealtimeGeneration(generation, nil, "", types.NewError(err, types.ErrorCodeDoRequestFailed))
		return nil, nil, realtimeProtocolError("invalid realtime event")
	}
	return generation, outgoing, nil
}

func ParseRealtimeUsage(value gjson.Result) *dto.RealtimeUsage {
	if !value.IsObject() {
		return nil
	}
	for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		v := value.Get(field)
		if !v.Exists() && field == "total_tokens" {
			continue
		}
		if v.Type != gjson.Number || v.Num < 0 || v.Num > float64(common.MaxQuota) || math.Trunc(v.Num) != v.Num {
			return nil
		}
	}
	var usage dto.RealtimeUsage
	if common.Unmarshal([]byte(value.Raw), &usage) != nil {
		return nil
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.InputTokens > common.MaxQuota-usage.OutputTokens || usage.TotalTokens != usage.InputTokens+usage.OutputTokens {
		return nil
	}
	// Realtime mixes text, audio and image input. Missing modality counts
	// cannot safely be reclassified as cheap text. ASR output is always text;
	// its official token-usage schema deliberately has no output breakdown.
	if value.Get("type").String() == "tokens" && !value.Get("output_token_details").Exists() {
		usage.OutputTokenDetails.TextTokens = usage.OutputTokens
	}
	input, output := usage.InputTokenDetails, usage.OutputTokenDetails
	if input.AudioTokens < 0 || input.TextTokens < 0 || input.ImageTokens < 0 || input.CachedTokens < 0 || output.AudioTokens < 0 || output.TextTokens < 0 {
		return nil
	}
	if input.AudioTokens > usage.InputTokens || input.TextTokens > usage.InputTokens-input.AudioTokens || input.ImageTokens != usage.InputTokens-input.AudioTokens-input.TextTokens || input.CachedTokens > usage.InputTokens || output.AudioTokens > usage.OutputTokens || output.TextTokens != usage.OutputTokens-output.AudioTokens {
		return nil
	}
	if details := input.CachedTokensDetails; details != nil {
		cached := 0
		for _, item := range []struct {
			value *int
			total int
		}{{details.AudioTokens, input.AudioTokens}, {details.TextTokens, input.TextTokens}, {details.ImageTokens, input.ImageTokens}} {
			if item.value != nil {
				if *item.value < 0 || *item.value > item.total || *item.value > input.CachedTokens-cached {
					return nil
				}
				cached += *item.value
			}
		}
	}
	return &usage
}
func finishRealtimeGeneration(generation *realtimeGeneration, usage *dto.RealtimeUsage, status string, apiErr *types.NewAPIError) {
	if generation == nil || generation.context == nil {
		return
	}
	c, info := generation.context, generation.info
	if info.ChannelLimitAttempt != nil && info.ChannelLimitAttempt.Finished {
		return
	}
	switch status {
	case "completed":
		info.StreamStatus.MarkCompleted()
	case "failed":
		info.StreamStatus.MarkFailed("upstream_failure", "server_error", http.StatusBadGateway)
	case "cancelled":
		info.StreamStatus.MarkCancelled()
	case "incomplete":
		info.StreamStatus.MarkIncomplete("upstream_incomplete")
	}
	if status == "completed" || status == "failed" || status == "cancelled" || status == "incomplete" {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	}
	if generation.billed {
		// Duration-based ASR was settled independently of text-token usage.
	} else if usage != nil {
		service.PostWssConsumeQuota(c, info, info.OriginModelName, usage, nil)
	} else if status == "completed" || status == "incomplete" {
		// A delivered response with unusable usage retains the frozen customer
		// reservation. Procurement and text-token actuals remain unknown.
		service.PostWssConsumeQuota(c, info, info.OriginModelName, nil, nil)
	} else if info.Billing != nil {
		info.Billing.Refund(c)
	}
	service.FinishSchedulingAttempt(c, info, apiErr)
	service.FinishOverviewAttempt(c, info, apiErr)
	if err := service.FinishChannelLimitAttempt(c, info, apiErr); err != nil {
		common.SysError("realtime channel settlement failed: " + err.Error())
	}
	service.CompleteOverviewRelay(info)
	service.EndSchedulingRequest(c)
}
