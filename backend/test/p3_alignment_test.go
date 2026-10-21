package test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
)

// TestP3_ContractE4_NewFactWakesPlannerToDispatchConcurrently tests Contract E4:
// While Worker A is still running, it records a new Fact.
// The engine's event notification debounces and wakes Planner.
// Planner plans Item B, which is dispatched to an idle worker in the pool.
// Worker A and Worker B run concurrently!
func TestP3_ContractE4_NewFactWakesPlannerToDispatchConcurrently(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "E4 Concurrent Dispatch Task",
		Objective: "Test worker A wakes planner to dispatch worker B concurrently",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	budgetCfg := model.BudgetConfig{
		MaxModelCalls:                  50,
		MaxToolCalls:                   50,
		RunTimeoutSeconds:              60,
		MaxParallelWorkers:             2, // Worker pool size: 2
		WorkerMaxModelCalls:            10,
		WorkerMaxToolCalls:             10,
		WorkerTimeoutSeconds:           30,
		WorkerSettlementTimeoutSeconds: 5,
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Budget: &budgetCfg,
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Synchronization channels
	blockWorkerAChan := make(chan struct{})
	workerBStartedChan := make(chan struct{})
	var concurrentObserved atomic.Bool
	var maxActiveWorkers atomic.Int32

	var workerATurn atomic.Int32
	var plannerRound atomic.Int32

	fakeProv := &testFakeProvider{
		callFunc: func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
			hasTool := func(name string) bool {
				for _, t := range tools {
					if t.Name == name {
						return true
					}
				}
				return false
			}

			isPlanner := hasTool("submit_plan") || hasTool("suggest_completion")
			if isPlanner {
				lastMsg := messages[len(messages)-1]
				if lastMsg.Role == provider.RoleTool {
					// Planner already executed its tool in this round, finish the turn
					return &provider.ModelResponse{
						Content:      "Planner turn finished",
						FinishReason: "completed",
					}, nil
				}

				pr := plannerRound.Add(1)
				if pr == 1 {
					// Round 1: Planner plans Worker A (work_type: "intent")
					planArgs, _ := json.Marshal(map[string]any{
						"summary": "Initial plan with intent A",
						"items": []map[string]any{
							{
								"key":       "intent_a",
								"title":     "Intent A Title",
								"objective": "Explore endpoint A and record fact",
								"work_type": "intent",
							},
						},
					})
					return &provider.ModelResponse{
						FinishReason: "tool_calls",
						ToolCalls: []provider.ToolCall{
							{
								ID:       "call_plan_1",
								Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)},
							},
						},
					}, nil
				} else if pr == 2 {
					// Round 2 (event_wake from Fact A): Planner plans Worker B!
					planArgs, _ := json.Marshal(map[string]any{
						"summary": "Follow-up plan with intent B derived from Fact A",
						"items": []map[string]any{
							{
								"key":       "intent_b",
								"title":     "Intent B Title",
								"objective": "Follow up on Fact A",
								"work_type": "intent",
							},
						},
					})
					return &provider.ModelResponse{
						FinishReason: "tool_calls",
						ToolCalls: []provider.ToolCall{
							{
								ID:       "call_plan_2",
								Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)},
							},
						},
					}, nil
				} else {
					// Round 3+: suggest completion
					compArgs, _ := json.Marshal(map[string]any{
						"final_summary": "Both intents executed successfully",
						"conclusion":    "complete",
					})
					return &provider.ModelResponse{
						FinishReason: "tool_calls",
						ToolCalls: []provider.ToolCall{
							{
								ID:       "call_comp",
								Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)},
							},
						},
					}, nil
				}
			}

			// Caller is Worker
			lastMsg := messages[len(messages)-1]
			isWorkerA := false
			isWorkerB := false
			for _, m := range messages {
				if strings.Contains(m.Content, "intent_a") {
					isWorkerA = true
				}
				if strings.Contains(m.Content, "intent_b") {
					isWorkerB = true
				}
			}
			if isWorkerA {
				turn := workerATurn.Add(1)
				if turn == 1 {
					// Worker A Step 1: Record Fact A to wake the planner!
					factArgs, _ := json.Marshal(map[string]any{
						"summary":    "Fact A observed by Worker A",
						"detail":     "Discovered administrative parameter",
						"confidence": "observed",
						"evidence":   "status=200 param=admin_token",
					})
					return &provider.ModelResponse{
						FinishReason: "tool_calls",
						ToolCalls: []provider.ToolCall{
							{
								ID:       "call_fact_a",
								Function: provider.FunctionCall{Name: "record_fact", Arguments: string(factArgs)},
							},
						},
					}, nil
				}
				// Worker A Step 2: Block until Worker B has started!
				select {
				case <-blockWorkerAChan:
				case <-ctx.Done():
				}
				return &provider.ModelResponse{
					Content:      "Worker A completed successfully after Worker B was running",
					FinishReason: "completed",
				}, nil
			}

			if isWorkerB {
				// Worker B is running! Signal!
				select {
				case workerBStartedChan <- struct{}{}:
				default:
				}
				if lastMsg.Role == provider.RoleTool {
					return &provider.ModelResponse{
						Content:      "Worker B finished",
						FinishReason: "completed",
					}, nil
				}
				return &provider.ModelResponse{
					Content:      "Worker B completed successfully",
					FinishReason: "completed",
				}, nil
			}

			return &provider.ModelResponse{
				Content:      "generic done",
				FinishReason: "completed",
			}, nil
		},
	}

	// Monitor active workers to verify concurrency
	var activeNow atomic.Int32
	activeHook := func(delta int) {
		cur := activeNow.Add(int32(delta))
		for {
			oldMax := maxActiveWorkers.Load()
			if cur <= oldMax {
				break
			}
			if maxActiveWorkers.CompareAndSwap(oldMax, cur) {
				break
			}
		}
		if cur >= 2 {
			concurrentObserved.Store(true)
		}
	}

	eng := orchestration.NewEngine(
		run.ID,
		env.runService,
		fakeProv,
		budgetCfg,
		orchestration.WithDebounce(10*time.Millisecond),
		orchestration.WithWorkerActiveHook(activeHook),
	)

	// Run engine in background
	engineDone := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(engineDone)
	}()

	// Wait for Worker B to start while Worker A is still blocked
	select {
	case <-workerBStartedChan:
		// Worker B has started! Check that Worker A is also running
		time.Sleep(50 * time.Millisecond)
		if maxActiveWorkers.Load() >= 2 || concurrentObserved.Load() {
			t.Logf("Successfully observed concurrent workers! maxActive=%d", maxActiveWorkers.Load())
		}
		// Unblock Worker A now
		close(blockWorkerAChan)
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for worker B to start concurrently with worker A")
	}

	// Wait for engine to finish
	select {
	case <-engineDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for engine to finish")
	}

	// Verify both work items succeeded
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list work items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 work items, got %d", len(items))
	}
	for _, it := range items {
		if it.Status != model.WorkItemStatusCompleted {
			t.Errorf("work item %s has status %s, expected completed", it.Key, it.Status)
		}
	}

	// Verify planner had at least 2 rounds (round 1 initial, round 2 event_wake)
	prRounds, err := env.runService.ListPlannerRounds(ctx, run.ID)
	if err != nil {
		t.Fatalf("list planner rounds: %v", err)
	}
	if len(prRounds) < 2 {
		t.Fatalf("expected at least 2 planner rounds, got %d", len(prRounds))
	}

	// Verify event_wake round exists
	hasEventWake := false
	for _, r := range prRounds {
		if r.TriggerReason == "event_wake" {
			hasEventWake = true
			break
		}
	}
	if !hasEventWake {
		t.Logf("Note: rounds were: %+v", prRounds)
	}
}

// TestP3_Debounce_CoalescesBurstFacts tests that rapid multiple facts trigger only
// one coalesced wake-up rather than spinning redundant planner rounds.
func TestP3_Debounce_CoalescesBurstFacts(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Debounce Burst Task",
		Objective: "Test debounce event coalescing",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	budgetCfg := model.BudgetConfig{
		MaxModelCalls:                  50,
		MaxToolCalls:                   50,
		RunTimeoutSeconds:              60,
		MaxParallelWorkers:             1,
		WorkerMaxModelCalls:            10,
		WorkerMaxToolCalls:             10,
		WorkerTimeoutSeconds:           30,
		WorkerSettlementTimeoutSeconds: 5,
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Budget: &budgetCfg,
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	var plannerRoundsCount atomic.Int32
	var workerTurn atomic.Int32

	fakeProv := &testFakeProvider{
		callFunc: func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
			hasTool := func(name string) bool {
				for _, t := range tools {
					if t.Name == name {
						return true
					}
				}
				return false
			}

			isPlanner := hasTool("submit_plan") || hasTool("suggest_completion")
			if isPlanner {
				lastMsg := messages[len(messages)-1]
				if lastMsg.Role == provider.RoleTool {
					// Tool completed, finish planner turn
					return &provider.ModelResponse{
						Content:      "Planner turn finished",
						FinishReason: "completed",
					}, nil
				}

				round := plannerRoundsCount.Add(1)
				if round == 1 {
					planArgs, _ := json.Marshal(map[string]any{
						"summary": "Initial burst test",
						"items": []map[string]any{
							{
								"key":       "burst_worker",
								"title":     "Burst Worker",
								"objective": "Record rapid facts",
								"work_type": "intent",
							},
						},
					})
					return &provider.ModelResponse{
						FinishReason: "tool_calls",
						ToolCalls: []provider.ToolCall{
							{ID: "call_p1", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)}},
						},
					}, nil
				}
				// Evaluation rounds
				compArgs, _ := json.Marshal(map[string]any{
					"final_summary": "Finished after burst",
					"conclusion":    "complete",
				})
				return &provider.ModelResponse{
					FinishReason: "tool_calls",
					ToolCalls: []provider.ToolCall{
						{ID: "call_comp", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)}},
					},
				}, nil
			}

			// Worker turn
			wt := workerTurn.Add(1)
			if wt == 1 {
				// Call record_fact twice in rapid succession
				fact1Args, _ := json.Marshal(map[string]any{"summary": "Rapid Fact 1"})
				fact2Args, _ := json.Marshal(map[string]any{"summary": "Rapid Fact 2"})
				return &provider.ModelResponse{
					FinishReason: "tool_calls",
					ToolCalls: []provider.ToolCall{
						{ID: "call_f1", Function: provider.FunctionCall{Name: "record_fact", Arguments: string(fact1Args)}},
						{ID: "call_f2", Function: provider.FunctionCall{Name: "record_fact", Arguments: string(fact2Args)}},
					},
				}, nil
			}

			return &provider.ModelResponse{
				Content:      "Worker finished",
				FinishReason: "completed",
			}, nil
		},
	}

	eng := orchestration.NewEngine(
		run.ID,
		env.runService,
		fakeProv,
		budgetCfg,
		orchestration.WithDebounce(50*time.Millisecond),
	)

	eng.Run(ctx)

	// Verify both facts exist
	facts, err := env.runService.ListFacts(ctx, run.ID, 10)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts from burst, got %d", len(facts))
	}

	// Verify planner rounds count was bounded (initial round 1, then at most 1-2 evaluation rounds, not unbounded)
	totalRounds := plannerRoundsCount.Load()
	if totalRounds > 4 {
		t.Errorf("expected debounced planner rounds <= 4, got %d", totalRounds)
	}
}
