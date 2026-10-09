package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Accounting observations live in the primary database, even when relay logs
// use a separate database or are disabled. Pending rows retain crash evidence.
type OverviewAttemptEvent struct {
	ID                      string `gorm:"type:varchar(96);primaryKey"`
	RequestID               string `gorm:"type:varchar(64);index"`
	Day                     string `gorm:"type:varchar(10);index"`
	StartedAt               int64  `gorm:"index:idx_overview_projection,priority:2"`
	FinishedAt              int64
	Status                  string `gorm:"type:varchar(16);index:idx_overview_projection,priority:1"`
	ChannelID               int
	ChannelName             string `gorm:"type:varchar(191)"`
	GroupName               string `gorm:"type:varchar(191)"`
	ModelName               string `gorm:"type:varchar(191)"`
	CredentialID            string `gorm:"type:varchar(64)"`
	CredentialName          string `gorm:"type:varchar(191)"`
	CredentialVersionID     string `gorm:"type:varchar(64)"`
	BindingID               string `gorm:"type:varchar(64)"`
	SupplierID              string `gorm:"type:varchar(64)"`
	SupplierName            string `gorm:"type:varchar(191)"`
	OwnershipVersionID      string `gorm:"type:varchar(64)"`
	CostVersionID           string `gorm:"type:varchar(64)"`
	CredentialCostVersionID string `gorm:"type:varchar(64)"`
	ChannelCostVersionID    string `gorm:"type:varchar(64)"`
	TagsJSON                string `gorm:"type:text"`
	CostSource              string `gorm:"type:varchar(16)"`
	CostRatio               *float64
	BaseQuota               *float64
	QuotaPerUnit            float64
	RevenueQuota            int64
	RawRevenueQuota         int64
	RevenueUnitAmbiguous    bool
	CostQuota               float64
	CostKnown               bool
	RevenueConfirmed        bool
	BillingSource           string `gorm:"type:varchar(16)"`
	RequestComplete         bool
	IsFinal                 bool
	Success                 bool
	Failure                 bool
	LatencyMS               int64
}

// Sparse daily cube: one row per occupied procurement/routing/model cohort.
// Its unique hash avoids dialect-specific oversized composite indexes.
type OverviewDaily struct {
	ID                         string `gorm:"type:varchar(64);primaryKey"`
	Day                        string `gorm:"type:varchar(10);index:idx_overview_day;index:idx_overview_channel,priority:2;index:idx_overview_group,priority:2;index:idx_overview_key,priority:2;index:idx_overview_supplier,priority:2"`
	ChannelID                  int    `gorm:"index:idx_overview_channel,priority:1"`
	ChannelName                string `gorm:"type:varchar(191)"`
	GroupName                  string `gorm:"type:varchar(191);index:idx_overview_group,priority:1"`
	ModelName                  string `gorm:"type:varchar(191)"`
	CredentialID               string `gorm:"type:varchar(64);index:idx_overview_key,priority:1"`
	CredentialName             string `gorm:"type:varchar(191)"`
	SupplierID                 string `gorm:"type:varchar(64);index:idx_overview_supplier,priority:1"`
	SupplierName               string `gorm:"type:varchar(191)"`
	OwnershipVersionID         string `gorm:"type:varchar(64)"`
	CostVersionID              string `gorm:"type:varchar(64)"`
	QuotaPerUnit               float64
	Revenue                    int64
	Cost                       float64
	CompleteRevenue            int64
	CompleteCost               float64
	Requests                   int64
	Attempts                   int64
	Successes                  int64
	Failures                   int64
	UnconfirmedRevenueRequests int64
	SubscriptionRevenue        int64
	UnknownCostAttempts        int64
	CompleteRequests           int64
	CompleteAttempts           int64
	LatencyTotalMS             int64
	LatencySamples             int64
}

type OverviewCollectionState struct {
	ID             int `gorm:"primaryKey"`
	CollectedSince int64
}

var overviewWriteFailure atomic.Int64

func InitializeOverviewCollection(db *gorm.DB) error {
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&OverviewCollectionState{ID: 1, CollectedSince: time.Now().Unix()}).Error
}

func BeginOverviewAttempt(event *OverviewAttemptEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(event).Error
	if err != nil {
		overviewWriteFailure.Store(time.Now().Unix())
	}
	return err
}

// Completion is a durable outbox write. Projection is asynchronous and can
// recover on any node; it never adds a per-request aggregate lookup.
func CompleteOverviewRequest(events []OverviewAttemptEvent) error {
	if len(events) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, event := range events {
			pending := event
			pending.Status, pending.FinishedAt = "pending", 0
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&pending).Error; err != nil {
				return err
			}
			event.Status = "ready"
			if err := tx.Model(&OverviewAttemptEvent{}).Where("id = ? AND status = ?", event.ID, "pending").Select("*").Updates(&event).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		overviewWriteFailure.Store(time.Now().Unix())
	}
	return err
}

// The status compare-and-set and cube additions commit atomically, so workers
// on different nodes and crash replays cannot project an event twice.
func ProjectOverviewEvents(ctx context.Context, db *gorm.DB) (int, error) {
	var events []OverviewAttemptEvent
	if err := db.WithContext(ctx).Where("status = ?", "ready").Order("started_at ASC, id ASC").Limit(100).Find(&events).Error; err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	projected := 0
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := make([]string, len(events))
		for i := range events {
			ids[i] = events[i].ID
		}
		claim := tx.Model(&OverviewAttemptEvent{}).Where("id IN ? AND status = ?", ids, "ready").Update("status", "completed")
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return nil
		}
		if claim.RowsAffected != int64(len(events)) {
			return errOverviewClaimChanged
		}
		rows := make(map[string]*OverviewDaily, len(events))
		for _, event := range events {
			identity, err := common.Marshal([]any{event.Day, event.ChannelID, event.GroupName, event.ModelName, event.CredentialID, event.SupplierID, event.OwnershipVersionID, event.CostVersionID, event.QuotaPerUnit})
			if err != nil {
				return err
			}
			hash := sha256.Sum256(identity)
			row := OverviewDaily{ID: hex.EncodeToString(hash[:]), Day: event.Day, ChannelID: event.ChannelID, ChannelName: event.ChannelName, GroupName: event.GroupName, ModelName: event.ModelName, CredentialID: event.CredentialID, CredentialName: event.CredentialName, SupplierID: event.SupplierID, SupplierName: event.SupplierName, OwnershipVersionID: event.OwnershipVersionID, CostVersionID: event.CostVersionID, QuotaPerUnit: event.QuotaPerUnit, Revenue: event.RevenueQuota, Attempts: 1, LatencyTotalMS: event.LatencyMS, LatencySamples: 1}
			if event.CostKnown {
				row.Cost = event.CostQuota
			} else {
				row.UnknownCostAttempts = 1
			}
			if event.BillingSource == "subscription" {
				row.SubscriptionRevenue = row.Revenue
			}
			if event.IsFinal {
				if !event.RevenueConfirmed {
					row.UnconfirmedRevenueRequests = 1
				}
				row.Requests = 1
			}
			if event.Success {
				row.Successes = 1
			}
			if event.Failure {
				row.Failures = 1
			}
			if event.RequestComplete {
				row.CompleteAttempts = 1
				row.CompleteRevenue, row.CompleteCost = row.Revenue, row.Cost
				if event.IsFinal {
					row.CompleteRequests = 1
				}
			}
			if previous := rows[row.ID]; previous != nil {
				previous.Revenue += row.Revenue
				previous.Cost += row.Cost
				previous.CompleteRevenue += row.CompleteRevenue
				previous.CompleteCost += row.CompleteCost
				previous.Requests += row.Requests
				previous.Attempts += row.Attempts
				previous.Successes += row.Successes
				previous.Failures += row.Failures
				previous.UnknownCostAttempts += row.UnknownCostAttempts
				previous.UnconfirmedRevenueRequests += row.UnconfirmedRevenueRequests
				previous.SubscriptionRevenue += row.SubscriptionRevenue
				previous.CompleteRequests += row.CompleteRequests
				previous.CompleteAttempts += row.CompleteAttempts
				previous.LatencyTotalMS += row.LatencyTotalMS
				previous.LatencySamples += row.LatencySamples
			} else {
				rows[row.ID] = &row
			}
		}
		rowIDs := make([]string, 0, len(rows))
		for id := range rows {
			rowIDs = append(rowIDs, id)
		}
		slices.Sort(rowIDs)
		for _, id := range rowIDs {
			row := rows[id]
			updates := map[string]any{}
			// Addition uses explicit columns and bound values, portable to all
			// three primary database dialects. No raw consume-log JSON scan.
			for column, delta := range map[string]int64{"revenue": row.Revenue, "complete_revenue": row.CompleteRevenue, "requests": row.Requests, "attempts": row.Attempts, "successes": row.Successes, "failures": row.Failures, "unknown_cost_attempts": row.UnknownCostAttempts, "unconfirmed_revenue_requests": row.UnconfirmedRevenueRequests, "subscription_revenue": row.SubscriptionRevenue, "complete_requests": row.CompleteRequests, "complete_attempts": row.CompleteAttempts, "latency_total_ms": row.LatencyTotalMS, "latency_samples": row.LatencySamples} {
				updates[column] = gorm.Expr("? + ?", clause.Column{Table: tx.NamingStrategy.TableName("OverviewDaily"), Name: column}, delta)
			}
			for column, delta := range map[string]float64{"cost": row.Cost, "complete_cost": row.CompleteCost} {
				updates[column] = gorm.Expr("? + ?", clause.Column{Table: tx.NamingStrategy.TableName("OverviewDaily"), Name: column}, delta)
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.Assignments(updates)}).Create(row).Error; err != nil {
				return err
			}
		}
		projected = len(events)
		return nil
	})
	if errors.Is(err, errOverviewClaimChanged) {
		return 0, nil
	}
	return projected, err
}

var errOverviewClaimChanged = errors.New("overview projection claim changed")

var overviewProjectorOnce sync.Once

func StartOverviewProjector() {
	overviewProjectorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for range ticker.C {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				var err error
				for range 10 {
					var count int
					count, err = ProjectOverviewEvents(ctx, DB)
					if err != nil || count < 100 {
						break
					}
				}
				cancel()
				if err != nil {
					common.SysError("channel overview projection failed: " + err.Error())
				}
			}
		}()
	})
}

type OverviewMetrics struct {
	Revenue                    float64  `json:"revenue"`
	Cost                       float64  `json:"cost"`
	CompleteRevenue            float64  `json:"complete_revenue"`
	CompleteCost               float64  `json:"complete_cost"`
	Margin                     float64  `json:"margin"`
	MarginRate                 *float64 `json:"margin_rate"`
	Requests                   int64    `json:"requests"`
	Attempts                   int64    `json:"attempts"`
	Successes                  int64    `json:"successes"`
	Failures                   int64    `json:"failures"`
	UnconfirmedRevenueRequests int64    `json:"unconfirmed_revenue_requests"`
	SubscriptionRevenue        float64  `json:"subscription_revenue"`
	UnknownCostAttempts        int64    `json:"unknown_cost_attempts"`
	CompleteAttempts           int64    `json:"complete_attempts"`
	CompleteRequests           int64    `json:"complete_requests"`
	CostCoverage               *float64 `json:"cost_coverage"`
	Health                     *float64 `json:"health"`
	LatencyMS                  *float64 `json:"latency_ms"`
	LatencyTotalMS             int64    `json:"-"`
	LatencySamples             int64    `json:"-"`
}

type OverviewRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Day  string `json:"day,omitempty"`
	OverviewMetrics
}

type OverviewOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type OverviewResult struct {
	Dimension      string           `json:"dimension"`
	Days           int              `json:"days"`
	Currency       string           `json:"currency"`
	RefreshedAt    int64            `json:"refreshed_at"`
	CollectedSince *int64           `json:"collected_since"`
	Partial        bool             `json:"partial"`
	Scope          string           `json:"scope"`
	Summary        OverviewMetrics  `json:"summary"`
	Daily          []OverviewRow    `json:"daily"`
	Ranking        []OverviewRow    `json:"ranking"`
	Distribution   []OverviewRow    `json:"distribution"`
	Models         []OverviewRow    `json:"models"`
	Options        []OverviewOption `json:"options"`
	Limits         map[string]int   `json:"limits"`
	Warnings       []string         `json:"warnings"`
}

// Complete-set margin must never subtract a partially known cost from total
// revenue. A zero revenue cohort still has a meaningful margin, no margin rate.
func (m *OverviewMetrics) Finalize() {
	m.Margin = m.CompleteRevenue - m.CompleteCost
	if m.CompleteRevenue != 0 {
		value := m.Margin / m.CompleteRevenue * 100
		m.MarginRate = &value
	}
	if m.Attempts > 0 {
		value := float64(m.Attempts-m.UnknownCostAttempts) / float64(m.Attempts) * 100
		m.CostCoverage = &value
	}
	if ended := m.Successes + m.Failures; ended > 0 {
		value := float64(m.Successes) / float64(ended) * 100
		m.Health = &value
	}
	if m.LatencySamples > 0 {
		value := float64(m.LatencyTotalMS) / float64(m.LatencySamples)
		m.LatencyMS = &value
	}
}

func GetChannelOverview(ctx context.Context, dimension string, days int, objectID string) (*OverviewResult, error) {
	column, nameColumn, distributionColumn, distributionName := "", "", "", ""
	switch dimension {
	case "channel":
		column, nameColumn, distributionColumn, distributionName = "channel_id", "channel_name", "group_name", "group_name"
	case "group":
		column, nameColumn, distributionColumn, distributionName = "group_name", "group_name", "channel_id", "channel_name"
	case "key":
		column, nameColumn, distributionColumn, distributionName = "credential_id", "credential_name", "channel_id", "channel_name"
	case "supplier":
		column, nameColumn, distributionColumn, distributionName = "supplier_id", "supplier_name", "credential_id", "credential_name"
	default:
		return nil, errors.New("unsupported dimension")
	}
	if days != 7 && days != 30 && days != 90 {
		return nil, errors.New("unsupported period")
	}
	if len(objectID) > 191 {
		return nil, errors.New("invalid object id")
	}
	now := time.Now().UTC()
	start := now.AddDate(0, 0, -days+1).Format("2006-01-02")
	end := now.Format("2006-01-02")
	result := &OverviewResult{Dimension: dimension, Days: days, Currency: "USD", RefreshedAt: now.Unix(), Scope: "synchronous_relay", Daily: []OverviewRow{}, Ranking: []OverviewRow{}, Distribution: []OverviewRow{}, Models: []OverviewRow{}, Options: []OverviewOption{}, Warnings: []string{}, Limits: map[string]int{"max_days": 90, "max_rows": 100}}
	// Main DB transaction gives one snapshot to all six business queries.
	options := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		// The SQLite driver uses BEGIN IMMEDIATE for ordinary transactions.
		// ReadOnly selects deferred BEGIN, allowing WAL writers during reports.
		options = &sql.TxOptions{ReadOnly: true}
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state struct {
			CollectedSince int64
			Pending        bool
		}
		pending := tx.Model(&OverviewAttemptEvent{}).Select("1").Where("status IN (?, ?)", "pending", "ready").Limit(1)
		if err := tx.Model(&OverviewCollectionState{}).Select("collected_since, EXISTS(?) AS pending", pending).Where("id = ?", 1).Scan(&state).Error; err != nil {
			return err
		}
		if state.CollectedSince > 0 {
			result.CollectedSince = &state.CollectedSince
		}
		if state.Pending {
			result.Partial = true
			result.Warnings = append(result.Warnings, "Unfinished attempts are excluded; collection may be incomplete")
		}
		sums := "COALESCE(SUM(revenue * 1.0 / NULLIF(quota_per_unit,0)),0) AS revenue, COALESCE(SUM(cost * 1.0 / NULLIF(quota_per_unit,0)),0) AS cost, COALESCE(SUM(complete_revenue * 1.0 / NULLIF(quota_per_unit,0)),0) AS complete_revenue, COALESCE(SUM(complete_cost * 1.0 / NULLIF(quota_per_unit,0)),0) AS complete_cost, COALESCE(SUM(subscription_revenue * 1.0 / NULLIF(quota_per_unit,0)),0) AS subscription_revenue, COALESCE(SUM(unconfirmed_revenue_requests),0) AS unconfirmed_revenue_requests, COALESCE(SUM(requests),0) AS requests, COALESCE(SUM(attempts),0) AS attempts, COALESCE(SUM(successes),0) AS successes, COALESCE(SUM(failures),0) AS failures, COALESCE(SUM(unknown_cost_attempts),0) AS unknown_cost_attempts, COALESCE(SUM(complete_attempts),0) AS complete_attempts, COALESCE(SUM(complete_requests),0) AS complete_requests, COALESCE(SUM(latency_total_ms),0) AS latency_total_ms, COALESCE(SUM(latency_samples),0) AS latency_samples"
		base := tx.Model(&OverviewDaily{}).Where("day >= ? AND day <= ?", start, end)
		filtered := base.Session(&gorm.Session{})
		if objectID != "" {
			filterID := objectID
			if (dimension == "key" || dimension == "supplier") && objectID == "__unassigned__" {
				filterID = ""
			}
			filtered = filtered.Where(column+" = ?", filterID)
		}
		if err := filtered.Session(&gorm.Session{}).Select("day, " + sums).Group("day").Order("day ASC").Limit(90).Scan(&result.Daily).Error; err != nil {
			return err
		}
		if err := filtered.Session(&gorm.Session{}).Select(column + " AS id, MAX(" + nameColumn + ") AS name, " + sums).Group(column).Order("SUM(revenue * 1.0 / NULLIF(quota_per_unit,0)) DESC, " + column + " ASC").Limit(100).Scan(&result.Ranking).Error; err != nil {
			return err
		}
		if err := filtered.Session(&gorm.Session{}).Select("day, " + distributionColumn + " AS id, MAX(" + distributionName + ") AS name, " + sums).Group("day, " + distributionColumn).Order("SUM(revenue * 1.0 / NULLIF(quota_per_unit,0)) DESC, day ASC, " + distributionColumn + " ASC").Limit(901).Scan(&result.Distribution).Error; err != nil {
			return err
		}
		if err := filtered.Session(&gorm.Session{}).Select("model_name AS id, model_name AS name, " + sums).Group("model_name").Order("SUM(attempts) DESC, model_name ASC").Limit(100).Scan(&result.Models).Error; err != nil {
			return err
		}
		if err := base.Session(&gorm.Session{}).Select(column + " AS id, MAX(" + nameColumn + ") AS name").Group(column).Order(column + " ASC").Limit(101).Scan(&result.Options).Error; err != nil {
			return err
		}
		return nil
	}, options)
	if err != nil {
		return nil, err
	}
	if len(result.Distribution) > 900 {
		result.Distribution = result.Distribution[:900]
		result.Warnings = append(result.Warnings, "Distribution is limited to the top 900 daily cohorts")
	}
	if len(result.Options) > 100 {
		result.Options = result.Options[:100]
		result.Warnings = append(result.Warnings, "Object options are limited to 100; use a specific object ID for additional objects")
	}
	for _, row := range result.Daily {
		result.Summary.Revenue += row.Revenue
		result.Summary.Cost += row.Cost
		result.Summary.CompleteRevenue += row.CompleteRevenue
		result.Summary.CompleteCost += row.CompleteCost
		result.Summary.Requests += row.Requests
		result.Summary.Attempts += row.Attempts
		result.Summary.Successes += row.Successes
		result.Summary.Failures += row.Failures
		result.Summary.UnknownCostAttempts += row.UnknownCostAttempts
		result.Summary.UnconfirmedRevenueRequests += row.UnconfirmedRevenueRequests
		result.Summary.SubscriptionRevenue += row.SubscriptionRevenue
		result.Summary.CompleteRequests += row.CompleteRequests
		result.Summary.CompleteAttempts += row.CompleteAttempts
		result.Summary.LatencyTotalMS += row.LatencyTotalMS
		result.Summary.LatencySamples += row.LatencySamples
	}
	result.Summary.Finalize()
	for _, rows := range [][]OverviewRow{result.Daily, result.Ranking, result.Distribution, result.Models} {
		for i := range rows {
			rows[i].Finalize()
		}
	}
	if dimension == "key" || dimension == "supplier" {
		for i := range result.Ranking {
			if result.Ranking[i].ID == "" {
				result.Ranking[i].ID = "__unassigned__"
			}
		}
		for i := range result.Options {
			if result.Options[i].ID == "" {
				result.Options[i].ID = "__unassigned__"
			}
		}
	}
	if dimension == "supplier" {
		for i := range result.Distribution {
			if result.Distribution[i].ID == "" {
				result.Distribution[i].ID = "__unassigned__"
			}
		}
	}
	if result.Summary.UnknownCostAttempts > 0 {
		result.Warnings = append(result.Warnings, "Some costs are unknown; margin includes only requests with every attempt cost known")
	}
	if result.Summary.UnconfirmedRevenueRequests > 0 {
		result.Warnings = append(result.Warnings, "Some requests have unconfirmed funding or refunds and are excluded from margin")
	}
	if overviewWriteFailure.Load() > 0 {
		result.Partial = true
		result.Warnings = append(result.Warnings, "A collection write failed on this node; totals may be incomplete")
	}
	result.Warnings = append(result.Warnings, "Amounts are billed quota equivalents; subscription usage is not cash revenue")
	result.Warnings = append(result.Warnings, "Usage revenue excludes violation fees and cash receipts")
	result.Warnings = append(result.Warnings, "UTC daily buckets; asynchronous tasks and unsupported transports are not included")
	return result, nil
}
