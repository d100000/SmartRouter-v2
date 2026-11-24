package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
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
// A request is counted this way only when its single consume row has no
// anomaly and the scan will not meet another row of the same request: the
// collector asks the log database, by the request_id index, which requests of
// the page have later rows inside the scan range. The decision therefore
// depends on the logs alone and stays exact when a scan is resumed.

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

// errorAnalysisDimensions names the distributions one request counts toward.
func errorAnalysisDimensions(entry FailureLog, location *time.Location) map[string]string {
	day := time.Unix(entry.CreatedAt, 0).In(location)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
	return map[string]string{"model": entry.Model, "channel": strconv.Itoa(entry.Channel), "reason": errorAnalysisReason(entry), "timeline": strconv.FormatInt(day.Unix(), 10)}
}

// errorAnalysisCleanTally collects the clean requests of one page and adds
// them to the same count and distribution rows aggregation adds to.
type errorAnalysisCleanTally struct {
	counts map[string]int
	dims   map[string]*errorAnalysisCleanDim
}

type errorAnalysisCleanDim struct {
	kind, value string
	requests    int
}

func (t *errorAnalysisCleanTally) add(period string, dims map[string]string) {
	if t.counts == nil {
		t.counts, t.dims = map[string]int{}, map[string]*errorAnalysisCleanDim{}
	}
	t.counts[period]++
	for dimension, value := range dims {
		kind := "dist_" + period + "_" + dimension
		identity := kind + "\x00" + value
		if t.dims[identity] == nil {
			t.dims[identity] = &errorAnalysisCleanDim{kind: kind, value: value}
		}
		t.dims[identity].requests++
	}
}

func (t *errorAnalysisCleanTally) artifacts(tx *gorm.DB, taskID string) ([]ErrorAnalysisArtifact, error) {
	result := make([]ErrorAnalysisArtifact, 0, len(t.counts)+len(t.dims))
	for period, requests := range t.counts {
		var counts FailureCounts
		if err := loadErrorAnalysisArtifact(tx, taskID, "counts", period, &counts); err != nil {
			return nil, err
		}
		counts.Requests += requests
		artifact, err := newErrorAnalysisArtifact(taskID, "counts", period, counts)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	for _, dim := range t.dims {
		name := fmt.Sprintf("%x", sha256.Sum256([]byte(dim.value)))
		distribution := FailureDistribution{Name: dim.value}
		if err := loadErrorAnalysisArtifact(tx, taskID, dim.kind, name, &distribution); err != nil {
			return nil, err
		}
		distribution.Requests += dim.requests
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
