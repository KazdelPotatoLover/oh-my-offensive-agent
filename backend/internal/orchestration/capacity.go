package orchestration

import (
	"encoding/json"

	"agent-workbench/backend/internal/contract"
)

// WorkerFieldInJSON reports whether max_parallel_workers is present in a create
// body or execution snapshot. A missing key is not the same as a stored 1.
func WorkerFieldInJSON(raw []byte) (present bool, requested int) {
	if len(raw) == 0 {
		return false, 0
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return false, 0
	}
	for _, key := range []string{"budget"} {
		field, ok := body[key]
		if !ok {
			continue
		}
		if present, requested, ok = workerFieldInObject(field); ok {
			return present, requested
		}
	}
	if cfg, ok := body["config"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(cfg, &nested) == nil {
			if field, ok := nested["budget"]; ok {
				if present, requested, ok = workerFieldInObject(field); ok {
					return present, requested
				}
			}
		}
	}
	if ac, ok := body["agent_config"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(ac, &nested) == nil {
			if field, ok := nested["budget"]; ok {
				if present, requested, ok = workerFieldInObject(field); ok {
					return present, requested
				}
			}
		}
	}
	return false, 0
}

func workerFieldInObject(raw json.RawMessage) (bool, int, bool) {
	var budget map[string]json.RawMessage
	if json.Unmarshal(raw, &budget) != nil {
		return false, 0, false
	}
	field, ok := budget["max_parallel_workers"]
	if !ok {
		return false, 0, true
	}
	var n int
	if json.Unmarshal(field, &n) != nil {
		return true, 0, true
	}
	return true, n, true
}

// WorkersFromSnapshot applies the resume rule. A missing field stays at 1.
// Discovery mode is not a reason to rewrite a stored count.
func WorkersFromSnapshot(raw []byte) (int, error) {
	present, requested := WorkerFieldInJSON(raw)
	return contract.ResolveRequestedWorkers(present, requested, false, contract.MaxParallelWorkers)
}

// SnapshotRuntimeLimits reads optional concurrency and reserve fields. Missing
// values stay zero so older snapshots keep their previous admission behavior.
type SnapshotRuntimeLimits struct {
	ModelConcurrency int
	PlannerReserve   int
	MainReserve      int
}

func RuntimeLimitsFromSnapshot(raw []byte) SnapshotRuntimeLimits {
	var out SnapshotRuntimeLimits
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return out
	}
	apply := func(field json.RawMessage) {
		var budget map[string]json.RawMessage
		if json.Unmarshal(field, &budget) != nil {
			return
		}
		read := func(key string, dest *int) {
			if raw, ok := budget[key]; ok {
				_ = json.Unmarshal(raw, dest)
			}
		}
		read("max_parallel_model_calls", &out.ModelConcurrency)
		read("planner_reserved_model_calls", &out.PlannerReserve)
		read("main_reserved_model_calls", &out.MainReserve)
	}
	if field, ok := body["budget"]; ok {
		apply(field)
	}
	return out
}

// RecommendedExplorationTemplate is the measured starting grant. It is not
// applied to an existing snapshot and is not a silent budget increase.
//
// Local measurement, 2026-10-22, exclusive test database agent_workbench_ma1,
// mock provider, four authorized pages, requested N=4, grant 40s / 80 model
// calls: snapshot, effective capacity, and observed HTTP overlap were all 4.
// Twenty repetitions recorded 30 or 32 model_calls, peak overlap 4, and no
// duplicate attempts. The contract reserve of 16 planner calls stays inside
// that 80-call grant as the supervision margin. Do not raise MaxModelCalls to
// hide a scheduler failure, and do not encode origin pacing by lowering N.
func RecommendedExplorationTemplate() contract.BudgetTemplate {
	return contract.SuggestedExplorationTemplate()
}
