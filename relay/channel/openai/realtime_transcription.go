package openai

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type realtimeAudioSegment struct {
	audio                  []byte
	control                realtimeTurnControl
	itemID                 string
	contentIndex           int
	startMS, endMS         int64
	eventID, clientEventID string
}
type realtimeTranscriptionJob struct {
	id      string
	segment realtimeAudioSegment
}
type realtimeTranscriptFrame struct {
	jobID      string
	generation *realtimeGeneration
	body       []byte
	err        error
}
type realtimeTranscriber struct {
	buffer          []byte
	baseMS, totalMS float64
	lastCommittedMS float64
	confirmedMS     float64
	bytesPerSecond  int64
	manual          []realtimeAudioSegment
	ranges          map[string]*realtimeAudioSegment
	items           []realtimeAudioSegment
	jobs            map[string]*realtimeGeneration
	events          chan realtimeTranscriptFrame
	workers         sync.WaitGroup
	all             sync.Map
	queue           chan realtimeTranscriptionJob
	started         bool
	connection      *websocket.Conn
	legacy          bool
	configuration   string
	maxBytes        int64
}

func newRealtimeTranscriber(maxBytes int64) *realtimeTranscriber {
	return &realtimeTranscriber{ranges: map[string]*realtimeAudioSegment{}, jobs: map[string]*realtimeGeneration{}, events: make(chan realtimeTranscriptFrame, 16), queue: make(chan realtimeTranscriptionJob, 4), maxBytes: maxBytes}
}

// The main session has transcription disabled. Its VAD timestamps are relative
// to all audio written during this session, including configured padding. Only
// those observed boundaries are used to submit a separately admitted ASR turn.
func (t *realtimeTranscriber) append(event gjson.Result, control realtimeTurnControl) error {
	encoded := event.Get("audio").String()
	audio, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || control.inputBytesPerSecond <= 0 {
		return errors.New("invalid input audio")
	}
	durationMS := float64(len(audio)) * 1000 / float64(control.inputBytesPerSecond)
	if math.IsInf(durationMS, 0) || math.IsNaN(durationMS) {
		return errors.New("invalid input audio duration")
	}
	if control.transcription == "" {
		t.totalMS += durationMS
		t.baseMS = t.totalMS
		t.buffer = nil
		return nil
	}
	if t.bytesPerSecond != 0 && t.bytesPerSecond != control.inputBytesPerSecond && len(t.buffer) > 0 {
		return errors.New("clear the audio buffer before changing its format")
	}
	if int64(len(t.buffer))+int64(len(audio)) > t.maxBytes {
		return errors.New("audio buffer limit exceeded")
	}
	if len(t.buffer) == 0 {
		t.baseMS = t.totalMS
	}
	t.bytesPerSecond = control.inputBytesPerSecond
	t.buffer = append(t.buffer, audio...)
	t.totalMS += durationMS
	return nil
}
func (t *realtimeTranscriber) commit(control realtimeTurnControl, eventID, clientEventID string) error {
	if len(t.manual) >= 16 {
		return errors.New("too many pending audio commits")
	}
	segment := realtimeAudioSegment{control: control, eventID: eventID, clientEventID: clientEventID, endMS: int64(math.Ceil(t.totalMS))}
	if control.transcription != "" {
		start := max(int64(0), int64(math.Round((t.lastCommittedMS-t.baseMS)*float64(t.bytesPerSecond)/1000)))
		start = min(start, int64(len(t.buffer)))
		segment.audio = append([]byte(nil), t.buffer[start:]...)
	}
	t.manual = append(t.manual, segment)
	t.lastCommittedMS = t.totalMS
	return nil
}
func (t *realtimeTranscriber) rejectCommit(eventID string) (string, bool) {
	for index, segment := range t.manual {
		if segment.eventID != eventID {
			continue
		}
		t.manual = append(t.manual[:index], t.manual[index+1:]...)
		t.lastCommittedMS = t.confirmedMS
		for _, pending := range t.manual {
			t.lastCommittedMS = max(t.lastCommittedMS, float64(pending.endMS))
		}
		return segment.clientEventID, true
	}
	return "", false
}
func (t *realtimeTranscriber) isManualCommit(event gjson.Result) bool {
	segment := t.ranges[event.Get("item_id").String()]
	return len(t.manual) > 0 && (segment == nil || segment.endMS < 0)
}
func (t *realtimeTranscriber) clear() {
	t.buffer = nil
	t.baseMS = t.totalMS
	t.lastCommittedMS = t.totalMS
	t.confirmedMS = t.totalMS
	t.ranges = map[string]*realtimeAudioSegment{}
	t.manual = nil
}
func (t *realtimeTranscriber) item(event gjson.Result, control realtimeTurnControl, clientEventID string) error {
	if control.transcription == "" {
		return nil
	}
	segments := []realtimeAudioSegment{}
	retainedBytes := int64(0)
	for _, segment := range t.items {
		retainedBytes += int64(len(segment.audio))
	}
	for index, part := range event.Get("item.content").Array() {
		if part.Get("type").String() != "input_audio" {
			continue
		}
		audio, err := base64.StdEncoding.DecodeString(part.Get("audio").String())
		if err != nil || int64(len(audio))+retainedBytes > t.maxBytes || len(t.items)+len(segments) >= 16 {
			return errors.New("invalid input audio")
		}
		segments = append(segments, realtimeAudioSegment{audio: audio, control: control, itemID: event.Get("item.id").String(), contentIndex: index, eventID: event.Get("event_id").String(), clientEventID: clientEventID})
		retainedBytes += int64(len(audio))
	}
	t.items = append(t.items, segments...)
	return nil
}
func (t *realtimeTranscriber) rejectItem(eventID string) (string, bool) {
	clientID, found := "", false
	rest := t.items[:0]
	for _, segment := range t.items {
		if segment.eventID == eventID {
			clientID, found = segment.clientEventID, true
		} else {
			rest = append(rest, segment)
		}
	}
	t.items = rest
	return clientID, found
}
func (t *realtimeTranscriber) observe(c *gin.Context, connection *relaycommon.RelayInfo, event gjson.Result, control realtimeTurnControl, done <-chan struct{}) error {
	kind := event.Get("type").String()
	if kind == "input_audio_buffer.committed" && control.transcription == "" && len(t.ranges) == 0 && len(t.manual) == 0 {
		return nil
	}
	id := event.Get("item_id").String()
	switch kind {
	case "input_audio_buffer.speech_started":
		value := event.Get("audio_start_ms")
		if id == "" || value.Type != gjson.Number || value.Num < 0 || value.Num > t.totalMS || math.Trunc(value.Num) != value.Num {
			return errors.New("invalid upstream speech boundary")
		}
		if len(t.ranges) >= 16 {
			return errors.New("too many uncommitted speech turns")
		}
		t.ranges[id] = &realtimeAudioSegment{control: control, itemID: id, startMS: value.Int(), endMS: -1}
	case "input_audio_buffer.speech_stopped":
		segment := t.ranges[id]
		value := event.Get("audio_end_ms")
		if segment == nil || value.Type != gjson.Number || value.Num < float64(segment.startMS) || value.Num > t.totalMS+1 || math.Trunc(value.Num) != value.Num {
			return errors.New("invalid upstream speech boundary")
		}
		segment.endMS = value.Int()
	case "input_audio_buffer.committed":
		var segment realtimeAudioSegment
		if t.isManualCommit(event) {
			segment = t.manual[0]
			t.manual = t.manual[1:]
			segment.itemID = id
			delete(t.ranges, id)
		} else {
			observed := t.ranges[id]
			if observed == nil || observed.endMS < 0 {
				return errors.New("upstream did not identify the committed audio range")
			}
			segment = *observed
			delete(t.ranges, id)
			t.lastCommittedMS = float64(segment.endMS)
			if segment.control.transcription == "" {
				t.confirmedMS = max(t.confirmedMS, float64(segment.endMS))
				return nil
			}
			start := int64(math.Round((float64(segment.startMS) - t.baseMS) * float64(t.bytesPerSecond) / 1000))
			end := int64(math.Round((float64(segment.endMS) - t.baseMS) * float64(t.bytesPerSecond) / 1000))
			if start < 0 || end < start || end > int64(len(t.buffer)) {
				return errors.New("committed audio is outside the retained buffer")
			}
			segment.audio = append([]byte(nil), t.buffer[start:end]...)
		}
		t.confirmedMS = max(t.confirmedMS, float64(segment.endMS))
		if segment.control.transcription == "" {
			return nil
		}
		if err := t.start(c, connection, segment, done); err != nil {
			return err
		}
		// Keep overlap for the next VAD prefix, as well as all uncommitted audio.
		keepMS := max(float64(control.prefixPaddingMS)+1000, 10000)
		dropMS := float64(segment.endMS) - keepMS - t.baseMS
		if dropMS > 0 {
			drop := min(len(t.buffer), int(dropMS*float64(t.bytesPerSecond)/1000))
			t.buffer = append([]byte(nil), t.buffer[drop:]...)
			t.baseMS += float64(drop) * 1000 / float64(t.bytesPerSecond)
		}
	case "conversation.item.created", "conversation.item.added":
		if event.Get("item.role").String() != "user" || len(t.items) == 0 {
			return nil
		}
		itemID := event.Get("item.id").String()
		if itemID == "" {
			return nil
		}
		rest := t.items[:0]
		for _, segment := range t.items {
			if segment.itemID != itemID {
				rest = append(rest, segment)
				continue
			}
			segment.itemID = itemID
			if err := t.start(c, connection, segment, done); err != nil {
				return err
			}
		}
		t.items = rest
	}
	return nil
}
func (t *realtimeTranscriber) start(c *gin.Context, connection *relaycommon.RelayInfo, segment realtimeAudioSegment, done <-chan struct{}) error {
	if segment.control.transcription == "" {
		return nil
	}
	if segment.itemID == "" || len(segment.audio) == 0 {
		return errors.New("empty committed audio cannot be transcribed")
	}
	if len(t.jobs) >= 4 {
		return errors.New("too many pending transcription turns")
	}
	id := common.NewRequestId()
	t.jobs[id] = nil
	if !t.started {
		t.started = true
		t.workers.Go(func() {
			defer func() {
				if t.connection != nil {
					_ = t.connection.Close()
				}
			}()
			for {
				select {
				case <-done:
					return
				case job := <-t.queue:
					if c.Request.Context().Err() != nil {
						return
					}
					t.run(c, connection, job.id, job.segment, done)
				}
			}
		})
	}
	t.queue <- realtimeTranscriptionJob{id, segment}
	return nil
}
func (t *realtimeTranscriber) emit(frame realtimeTranscriptFrame, done <-chan struct{}) bool {
	select {
	case t.events <- frame:
		return true
	case <-done:
		return false
	}
}
func (t *realtimeTranscriber) run(parent *gin.Context, connection *relaycommon.RelayInfo, id string, segment realtimeAudioSegment, done <-chan struct{}) {
	var generation *realtimeGeneration
	emitted := false
	defer func() {
		if !emitted {
			if t.connection != nil {
				_ = t.connection.Close()
				t.connection = nil
				t.configuration = ""
			}
			t.emit(realtimeTranscriptFrame{jobID: id, generation: generation, err: errors.New("transcription connection ended without a result")}, done)
		}
	}()
	timeout := time.Duration(constant.StreamingTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent.Request.Context(), timeout)
	defer cancel()
	c := parent.Copy()
	c.Request = parent.Request.Clone(ctx)
	c.Set("realtime_transcription_session", true)
	dialInfo := *connection
	meta := *connection.ChannelMeta
	dialInfo.ChannelMeta = &meta
	dialInfo.ChannelBaseUrl = common.GetContextKeyString(parent, constant.ContextKeyChannelBaseUrl)
	dialInfo.OriginModelName, dialInfo.UpstreamModelName = segment.control.transcription, segment.control.transcription
	if helper.ModelMappedHelper(c, &dialInfo, nil) != nil {
		return
	}
	if t.connection == nil {
		adaptor := &Adaptor{}
		adaptor.Init(&dialInfo)
		conn, err := relaychannel.DoWssRequest(adaptor, c, &dialInfo, nil)
		if err != nil {
			return
		}
		t.connection = conn
		conn.SetReadLimit(t.maxBytes)
		deadline, _ := ctx.Deadline()
		_ = conn.SetReadDeadline(deadline)
		cancelClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer cancelClose()
		_, created, err := conn.ReadMessage()
		if err != nil {
			return
		}
		eventType := gjson.GetBytes(created, "type").String()
		t.legacy = eventType == "transcription_session.created"
		if eventType != "session.created" && !t.legacy {
			return
		}
	}
	conn := t.connection
	cancelClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer cancelClose()
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	var transcription map[string]any
	if common.Unmarshal([]byte(segment.control.transcriptionConfig.Raw), &transcription) != nil {
		return
	}
	transcription["model"] = dialInfo.UpstreamModelName
	input := map[string]any{"format": segment.control.inputFormat, "transcription": transcription, "turn_detection": nil}
	session := map[string]any{"type": "transcription", "audio": map[string]any{"input": input}}
	updateType := "session.update"
	if t.legacy {
		updateType = "transcription_session.update"
		session = map[string]any{"input_audio_format": segment.control.legacyInputFormat, "input_audio_transcription": transcription, "turn_detection": nil}
	}
	if segment.control.noiseReduction.Exists() {
		var noise any
		if common.Unmarshal([]byte(segment.control.noiseReduction.Raw), &noise) != nil {
			return
		}
		if t.legacy {
			session["input_audio_noise_reduction"] = noise
		} else {
			input["noise_reduction"] = noise
		}
	}
	if segment.control.include.Exists() {
		var include any
		if common.Unmarshal([]byte(segment.control.include.Raw), &include) != nil {
			return
		}
		session["include"] = include
	}
	update, _ := common.Marshal(map[string]any{"type": updateType, "session": session})
	if t.configuration != string(update) {
		if writeRealtimeFrame(conn, update) != nil {
			return
		}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			event := gjson.ParseBytes(raw)
			kind := event.Get("type").String()
			if kind == "error" {
				return
			}
			if kind == "session.updated" || kind == "transcription_session.updated" {
				vadPath, modelPath := "session.audio.input.turn_detection", "session.audio.input.transcription.model"
				if t.legacy {
					vadPath, modelPath = "session.turn_detection", "session.input_audio_transcription.model"
				}
				if !event.Get(vadPath).Exists() || event.Get(vadPath).Type != gjson.Null || event.Get(modelPath).String() != dialInfo.UpstreamModelName {
					return
				}
				t.configuration = string(update)
				break
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	body, _ := common.Marshal(map[string]any{"type": "response.create", "response": map[string]any{"model": segment.control.transcription}})
	seconds := float64(len(segment.audio)) / float64(segment.control.inputBytesPerSecond)
	generation, _, apiErr := prepareRealtimeGeneration(c, connection, body, gjson.Result{}, segment.control.transcription, "realtime_transcription", seconds)
	if apiErr != nil {
		emitted = t.emit(realtimeTranscriptFrame{jobID: id, err: errors.New("transcription capacity or authorization is unavailable")}, done)
		return
	}
	t.all.Store(id, generation)
	if !t.emit(realtimeTranscriptFrame{jobID: id, generation: generation}, done) {
		return
	}
	for offset := 0; offset < len(segment.audio); {
		end := min(offset+48*1024, len(segment.audio))
		payload, _ := common.Marshal(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(segment.audio[offset:end])})
		if writeRealtimeFrame(conn, payload) != nil {
			return
		}
		offset = end
	}
	if writeRealtimeFrame(conn, []byte(`{"type":"input_audio_buffer.commit"}`)) != nil {
		return
	}
	committedItemID := ""
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		event := gjson.ParseBytes(raw)
		kind := event.Get("type").String()
		if kind == "error" {
			return
		}
		if kind == "input_audio_buffer.committed" {
			committedItemID = event.Get("item_id").String()
			if committedItemID == "" {
				return
			}
			continue
		}
		if !strings.HasPrefix(kind, "conversation.item.input_audio_transcription.") {
			continue
		}
		if committedItemID == "" || event.Get("item_id").String() != committedItemID {
			continue
		}
		raw, _ = sjson.SetBytes(raw, "item_id", segment.itemID)
		raw, _ = sjson.SetBytes(raw, "content_index", segment.contentIndex)
		if !t.emit(realtimeTranscriptFrame{jobID: id, generation: generation, body: raw}, done) {
			return
		}
		if kind == "conversation.item.input_audio_transcription.completed" || kind == "conversation.item.input_audio_transcription.failed" {
			emitted = true
			return
		}
	}
}
