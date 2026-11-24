package service

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// Several small anomaly groups share one model call (a "pack"), filled up to
// the model's input budget, and packs run in parallel up to the preset's
// concurrency. A large group keeps its own call and the splitting rules of
// analyzeGroup. Each group's finding is saved on its own, so a resumed run
// only packs the groups still missing a finding.

const analysisFindingOutputTokens = 1500

func (w *analysisWorkflow) setPhase(phase, step string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Phase, w.state.CurrentStep = phase, step
}

// loadGroups returns every anomaly group, most affected first: the report
// lists the largest problems first and packs are filled in that order.
func (w *analysisWorkflow) loadGroups() ([]model.ErrorAnalysisGroup, error) {
	var groups []model.ErrorAnalysisGroup
	var afterID int64
	for {
		page, err := model.ListErrorAnalysisArtifacts(w.task.TaskID, "group", afterID, 200)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, artifact := range page {
			afterID = artifact.ID
			var group model.ErrorAnalysisGroup
			if err := common.UnmarshalJsonStr(string(artifact.Data), &group); err != nil {
				return nil, err
			}
			groups = append(groups, group)
		}
	}
	slices.SortStableFunc(groups, func(a, b model.ErrorAnalysisGroup) int {
		if a.Current.Affected != b.Current.Affected {
			return b.Current.Affected - a.Current.Affected
		}
		if a.Previous.Affected != b.Previous.Affected {
			return b.Previous.Affected - a.Previous.Affected
		}
		return strings.Compare(a.ID, b.ID)
	})
	return groups, nil
}

func (w *analysisWorkflow) analyzeGroups(groups []model.ErrorAnalysisGroup) ([]analysisFinding, error) {
	findings := make([]analysisFinding, len(groups))
	pending := []int{}
	for i, group := range groups {
		found, err := model.GetErrorAnalysisArtifact(w.task.TaskID, "finding", group.ID, &findings[i])
		if err != nil {
			return nil, err
		}
		if found {
			w.state.CompletedChunks++
		} else {
			pending = append(pending, i)
		}
	}
	if err := w.saveState(); err != nil {
		return nil, err
	}
	packs := w.buildPacks(groups, pending)
	limit := make(chan struct{}, analysisConcurrency(w.config))
	var wg sync.WaitGroup
	var failed error
	var failedMu sync.Mutex
	for _, pack := range packs {
		failedMu.Lock()
		stop := failed != nil
		failedMu.Unlock()
		if stop {
			break
		}
		limit <- struct{}{}
		wg.Go(func() {
			defer func() { <-limit }()
			err := func() (err error) {
				defer func() {
					if recover() != nil {
						err = &analysisFailure{Code: "analysis_internal_error"}
					}
				}()
				results, err := w.analyzePack(groups, pack)
				if err != nil {
					return err
				}
				for _, index := range pack {
					if err := w.saveArtifact("finding", groups[index].ID, results[index]); err != nil {
						return err
					}
				}
				w.mu.Lock()
				defer w.mu.Unlock()
				for _, index := range pack {
					findings[index] = results[index]
					w.state.CompletedChunks++
				}
				return w.saveState()
			}()
			if err != nil {
				failedMu.Lock()
				// Prefer the error that explains a stop over a sibling's cancellation.
				if failed == nil || errors.Is(failed, w.ctx.Err()) {
					failed = err
				}
				failedMu.Unlock()
			}
		})
	}
	wg.Wait()
	return findings, failed
}

// buildPacks groups pending anomaly groups into calls. A group using more
// than half the budget, or a pack that would need more output than reserved,
// starts a new pack.
func (w *analysisWorkflow) buildPacks(groups []model.ErrorAnalysisGroup, pending []int) [][]int {
	budget := analysisEvidenceBudget(w.config, w.systemPrompt(analysisPackInstruction)) * 4 / 5
	perPack := max(1, w.config.MaxOutputTokens/analysisFindingOutputTokens)
	var packs [][]int
	var current []int
	used := 0
	for _, index := range pending {
		group := groups[index]
		compactAnalysisGroup(&group)
		encoded, err := common.Marshal(w.groupInput(group))
		size := budget
		if err == nil {
			size = analysisTokenCount(w.config.Model, string(encoded))
		}
		if size > budget/2 {
			packs = append(packs, []int{index})
			continue
		}
		if len(current) > 0 && (used+size > budget || len(current) >= perPack) {
			packs = append(packs, current)
			current, used = nil, 0
		}
		current = append(current, index)
		used += size
	}
	if len(current) > 0 {
		packs = append(packs, current)
	}
	return packs
}

const analysisPackInstruction = "Return ONLY a JSON object {\"findings\":[...]} with exactly one entry per supplied group, in any order. Each entry has group_id (copied from the group's id), title:string, facts:string[], possible_causes:string[], next_checks:string[], missing_evidence:string[], evidence_ids:string[], follow_up_request_ids:string[]. Analyse each group on its own evidence and cite only request IDs from that group's samples. " + analysisFindingRules

type analysisPackFinding struct {
	GroupID string `json:"group_id"`
	analysisFinding
}

// analyzePack returns a finding for every group of the pack. A pack the model
// cannot complete is split; a single group falls back to analyzeGroup.
func (w *analysisWorkflow) analyzePack(groups []model.ErrorAnalysisGroup, pack []int) (map[int]analysisFinding, error) {
	results := map[int]analysisFinding{}
	if len(pack) == 1 {
		w.setPhase("analyzing", groups[pack[0]].Reason)
		finding, err := w.analyzeGroup(groups[pack[0]], 0)
		if err != nil {
			return nil, err
		}
		results[pack[0]] = finding
		return results, nil
	}
	w.setPhase("analyzing", fmt.Sprintf("%d groups", len(pack)))
	inputs := make([]analysisGroupInput, 0, len(pack))
	allowed := map[string][]string{}
	byID := map[string]int{}
	ids := make([]string, 0, len(pack))
	for _, index := range pack {
		group := groups[index]
		compactAnalysisGroup(&group)
		inputs = append(inputs, w.groupInput(group))
		allowed[group.ID] = analysisGroupRequestIDs(group)
		byID[group.ID] = index
		ids = append(ids, group.ID)
	}
	input, err := common.Marshal(map[string]any{"groups": inputs})
	if err != nil {
		return nil, err
	}
	decode := func(text string) (map[string]analysisFinding, error) {
		text = strings.TrimSpace(text)
		text = strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```")
		var payload struct {
			Findings []analysisPackFinding `json:"findings"`
		}
		if err := common.UnmarshalJsonStr(strings.TrimSpace(text), &payload); err != nil {
			return nil, &analysisFailure{Code: "analysis_invalid_finding", Retryable: true}
		}
		decoded := map[string]analysisFinding{}
		for _, item := range payload.Findings {
			if _, known := allowed[item.GroupID]; !known {
				return nil, &analysisFailure{Code: "analysis_invalid_finding", Retryable: true}
			}
			encoded, err := common.Marshal(item.analysisFinding)
			if err != nil {
				return nil, err
			}
			finding, err := decodeAnalysisFinding(string(encoded), allowed[item.GroupID])
			if err != nil {
				return nil, err
			}
			decoded[item.GroupID] = finding
		}
		if len(decoded) != len(pack) {
			return nil, &analysisFailure{Code: "analysis_invalid_finding", Retryable: true}
		}
		return decoded, nil
	}
	step := fmt.Sprintf("pack:%x", sha256.Sum256([]byte(strings.Join(ids, ","))))
	content, err := w.call(step, w.systemPrompt(analysisPackInstruction), string(input), func(text string) error { _, err := decode(text); return err })
	if err != nil {
		var failure *analysisFailure
		if !errors.As(err, &failure) || !slices.Contains([]string{"analysis_context_limit", "analysis_incomplete_report", "analysis_invalid_finding"}, failure.Code) {
			return nil, err
		}
		// The pack is too large or the model could not keep the groups apart:
		// analyse the halves separately.
		middle := len(pack) / 2
		for _, half := range [][]int{pack[:middle], pack[middle:]} {
			partial, err := w.analyzePack(groups, half)
			if err != nil {
				return nil, err
			}
			for index, finding := range partial {
				results[index] = finding
			}
		}
		return results, nil
	}
	decoded, err := decode(content)
	if err != nil {
		return nil, err
	}
	for id, finding := range decoded {
		index := byID[id]
		group := groups[index]
		compactAnalysisGroup(&group)
		if finding, err = w.followUp(group, finding, 0); err != nil {
			return nil, err
		}
		results[index] = finding
	}
	return results, nil
}

// analysisSeverity rates a group from its share of all requests and whether
// it touches billing. A model cannot change it.
func analysisSeverity(group model.ErrorAnalysisGroup, requests int) string {
	affected := group.Current.Affected
	if affected == 0 {
		return "low"
	}
	billing := group.Current.SettlementRisk + group.Current.UpstreamUsageUnbilled + group.Current.ChargedZeroOutput
	share := 0.0
	if requests > 0 {
		share = float64(affected) / float64(requests)
	}
	switch {
	case billing > 0 || share >= 0.05:
		return "critical"
	case share >= 0.01 || affected >= 50:
		return "high"
	case share >= 0.001 || affected >= 5:
		return "medium"
	default:
		return "low"
	}
}

var analysisSeverityRank = map[string]int{"": 0, "none": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}

func maxAnalysisSeverity(a, b string) string {
	if analysisSeverityRank[b] > analysisSeverityRank[a] {
		return b
	}
	return a
}

func analysisShareNames(shares []model.ErrorAnalysisShare) []string {
	names := make([]string, 0, min(len(shares), 5))
	for _, share := range shares[:min(len(shares), 5)] {
		names = append(names, share.Name)
	}
	return names
}

// analysisIssueBrief is the short form of a finding given to the final call.
type analysisIssueBrief struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Severity    string   `json:"severity"`
	Affected    int      `json:"current_affected"`
	Previous    int      `json:"previous_affected"`
	Channels    []string `json:"channels,omitempty"`
	Models      []string `json:"models,omitempty"`
	Facts       []string `json:"facts"`
	Causes      []string `json:"possible_causes,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

func analysisIssueBriefs(groups []model.ErrorAnalysisGroup, findings []analysisFinding, requests int) []analysisIssueBrief {
	briefs := make([]analysisIssueBrief, 0, len(groups))
	for i, group := range groups {
		finding := findings[i]
		briefs = append(briefs, analysisIssueBrief{
			ID: fmt.Sprintf("issue-%d", i+1), Title: finding.Title, Severity: analysisSeverity(group, requests), Affected: group.Current.Affected, Previous: group.Previous.Affected,
			Channels: analysisShareNames(group.Channels), Models: analysisShareNames(group.Models),
			Facts: finding.Facts[:min(3, len(finding.Facts))], Causes: finding.Causes[:min(2, len(finding.Causes))], EvidenceIDs: finding.EvidenceIDs[:min(4, len(finding.EvidenceIDs))],
		})
	}
	return briefs
}

func analysisAllEvidenceIDs(findings []analysisFinding) []string {
	var ids []string
	for _, finding := range findings {
		for _, id := range finding.EvidenceIDs {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

const analysisFinalInstruction = "Return ONLY a JSON object with summary:string, priority:object, actions:string[], limits:string[]. summary is ONE natural paragraph of 4-5 short sentences, about 200-300 Chinese characters when writing Chinese: the exact date range and analysed request count, affected count/rate, the dominant problem and where it concentrates, previous-period increases/decreases if comparable, and the first concrete check; if previous data is unavailable say so; do not say 'past seven days' unless the range is seven days. priority has title, facts, possible_causes, next_checks, missing_evidence, evidence_ids: the overall priority concern across issues, at most 6 short items per array, citing only supplied evidence IDs; never add counts from different issues. actions are 3-5 investigation steps ordered by priority, each naming the channel/model or evidence to inspect, the concrete check, and what would support or weaken the hypothesis, without numbering. limits are 2-3 short sentences on missing evidence that materially affects the diagnosis; supplied period boundaries and channel denominators are not missing evidence. Use channel_comparisons for channel denominators and rate changes. Categories overlap. Omit billing and refund disclaimers when no billing risk is indicated."

type analysisFinal struct {
	Summary  string           `json:"summary"`
	Priority *analysisFinding `json:"priority"`
	Actions  []string         `json:"actions"`
	Limits   []string         `json:"limits"`
}

// decodeAnalysisFinal accepts the JSON answer; prose from a model that ignored
// the format becomes the summary rather than failing the report. A priority
// citing unknown evidence is dropped.
func decodeAnalysisFinal(content string, allowed []string) analysisFinal {
	text := strings.TrimSpace(content)
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
	var final analysisFinal
	if err := common.UnmarshalJsonStr(text, &final); err != nil || strings.TrimSpace(final.Summary) == "" {
		return analysisFinal{Summary: content}
	}
	if final.Priority != nil {
		// Unknown citations are dropped rather than discarding the priority.
		final.Priority.EvidenceIDs = slices.DeleteFunc(final.Priority.EvidenceIDs, func(id string) bool { return !slices.Contains(allowed, id) })
		final.Priority.FollowUpIDs = nil
		encoded, err := common.Marshal(final.Priority)
		if err != nil {
			final.Priority = nil
		} else if priority, err := decodeAnalysisFinding(string(encoded), allowed); err != nil {
			final.Priority = nil
		} else {
			final.Priority = &priority
		}
	}
	clean := func(items []string, limit int) []string {
		out := []string{}
		for _, item := range items {
			if item = strings.TrimSpace(item); item != "" && len(out) < limit {
				out = append(out, item)
			}
		}
		return out
	}
	final.Actions, final.Limits = clean(final.Actions, 6), clean(final.Limits, 4)
	return final
}

// saveArtifact serialises the workflow's database writes. Parallel workers
// would otherwise contend for SQLite's single writer.
func (w *analysisWorkflow) saveArtifact(kind, name string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return model.SaveErrorAnalysisArtifact(w.task.TaskID, w.runner, kind, name, value)
}
