package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Most logged requests succeed on their first attempt. Such a request adds one
// to the request totals and nothing else, so it is counted while the logs are
// scanned instead of being stored as its own evidence row. Only requests with
// an anomaly or more than one log row are kept for aggregation and the
// evidence dialog. This keeps main-database writes proportional to failures.
//
// A clean request is recognised when its single consume row arrives and no
// earlier row of the same request exists. If a later row of that request
// appears after all, the count is withdrawn and the request is stored with
// both rows. Logs are scanned in ID order, so retries normally precede the
// final consume row; the remembered window covers late rows within one run.

const errorAnalysisCleanMemory = 50_000

type errorAnalysisCleanEntry struct {
	entry  FailureLog
	period string
	dims   map[string]string
}

type errorAnalysisCleanWindow struct {
	order   []string
	entries map[string]errorAnalysisCleanEntry
}

var errorAnalysisCleanWindows = struct {
	sync.Mutex
	byTask map[string]*errorAnalysisCleanWindow
}{byTask: map[string]*errorAnalysisCleanWindow{}}

func errorAnalysisCleanWindowFor(generation string) *errorAnalysisCleanWindow {
	errorAnalysisCleanWindows.Lock()
	defer errorAnalysisCleanWindows.Unlock()
	window := errorAnalysisCleanWindows.byTask[generation]
	if window == nil {
		window = &errorAnalysisCleanWindow{entries: map[string]errorAnalysisCleanEntry{}}
		errorAnalysisCleanWindows.byTask[generation] = window
	}
	return window
}

func forgetErrorAnalysisCleanWindow(generation string) {
	errorAnalysisCleanWindows.Lock()
	delete(errorAnalysisCleanWindows.byTask, generation)
	errorAnalysisCleanWindows.Unlock()
}

func (w *errorAnalysisCleanWindow) remember(key string, value errorAnalysisCleanEntry) {
	if _, exists := w.entries[key]; !exists {
		w.order = append(w.order, key)
	}
	w.entries[key] = value
	for len(w.order) > errorAnalysisCleanMemory {
		delete(w.entries, w.order[0])
		w.order = w.order[1:]
	}
}

func errorAnalysisReason(entry FailureLog) string {
	reason := entry.Diagnostic.ErrorCode
	if reason == "" {
		reason = entry.Diagnostic.EndReason
	}
	if reason == "" {
		reason = strconv.Itoa(entry.Diagnostic.StatusCode)
	}
	return reason
}

func errorAnalysisDimensions(entry FailureLog, location *time.Location) map[string]string {
	day := time.Unix(entry.CreatedAt, 0).In(location)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
	return map[string]string{"model": entry.Model, "channel": strconv.Itoa(entry.Channel), "reason": errorAnalysisReason(entry), "timeline": strconv.FormatInt(day.Unix(), 10)}
}

// errorAnalysisCleanTally collects request-count changes for one page and
// writes them to the same count and distribution rows aggregation adds to.
type errorAnalysisCleanTally struct {
	counts map[string]int
	dims   map[string]*errorAnalysisCleanDim
}

type errorAnalysisCleanDim struct {
	kind, value string
	delta       int
}

func newErrorAnalysisCleanTally() *errorAnalysisCleanTally {
	return &errorAnalysisCleanTally{counts: map[string]int{}, dims: map[string]*errorAnalysisCleanDim{}}
}

func (t *errorAnalysisCleanTally) add(clean errorAnalysisCleanEntry, delta int) {
	t.counts[clean.period] += delta
	for dimension, value := range clean.dims {
		kind := "dist_" + clean.period + "_" + dimension
		identity := kind + "\x00" + value
		if t.dims[identity] == nil {
			t.dims[identity] = &errorAnalysisCleanDim{kind: kind, value: value}
		}
		t.dims[identity].delta += delta
	}
}

func (t *errorAnalysisCleanTally) artifacts(tx *gorm.DB, taskID string) ([]ErrorAnalysisArtifact, error) {
	result := []ErrorAnalysisArtifact{}
	for period, delta := range t.counts {
		if delta == 0 {
			continue
		}
		var counts FailureCounts
		if err := loadErrorAnalysisArtifact(tx, taskID, "counts", period, &counts); err != nil {
			return nil, err
		}
		counts.Requests += delta
		artifact, err := newErrorAnalysisArtifact(taskID, "counts", period, counts)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	for _, dim := range t.dims {
		if dim.delta == 0 {
			continue
		}
		name := fmt.Sprintf("%x", sha256.Sum256([]byte(dim.value)))
		distribution := FailureDistribution{Name: dim.value}
		if err := loadErrorAnalysisArtifact(tx, taskID, dim.kind, name, &distribution); err != nil {
			return nil, err
		}
		distribution.Requests += dim.delta
		artifact, err := newErrorAnalysisArtifact(taskID, dim.kind, name, distribution)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, nil
}

func loadErrorAnalysisArtifact(tx *gorm.DB, taskID, kind, name string, value any) error {
	var stored ErrorAnalysisArtifact
	err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, kind, name).Take(&stored).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return common.UnmarshalJsonStr(string(stored.Data), value)
}
