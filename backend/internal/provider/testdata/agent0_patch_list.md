# Agent 0 integration patches

Agent 2 does not edit Engine, service/run.go, or the server entrypoint.

## service/run.go

After the existing `agent_config` snapshot is built, attach a non-sensitive role snapshot when `execution_input.agents` is present. Missing `agents` keeps the historical single `agent_config`.

- Insertion point: the `executionInputMap["agent_config"] = agentConfig` block.
- Dependency commit: Agent 2 provider commit that adds `StaticRuntimeResolver` and `nonSensitiveSnapshot`.
- Required interface: `contract.ParseAgentsConfig`, `provider.StaticRuntimeResolver.Resolve`.
- Do not copy `LLM_API_KEY` or Authorization into the snapshot.

## orchestration/engine.go

Pass one shared limiter and an opt-in recovery policy into every `agent.NewSessionRunner` call. Do not add a second engine or a serial fallback.

- Insertion point: each `agent.NewSessionRunner(...)` option list.
- Options: `agent.WithRequestLimiter(sharedLimiter)`, `agent.WithModelRecovery(policy)` only when recovery is explicitly enabled.
- `sharedLimiter` comes from server config `LLM_MAX_INFLIGHT`. `<= 0` means unlimited.
- Worker pool `N` stays independent of this limit.

## cmd/server

Read `LLM_MAX_INFLIGHT` and `LLM_MAX_OUTPUT_TOKENS`. Reject values above 8192. Do not default to an unlimited output token budget.

No migration 027. Diagnostics fit `model_calls.error_message` and `request_id`.

Verifier sessions already require a running `finding_retests` row in `lockSessionAdmission`. Do not bypass that check when attaching a role snapshot. `RoleCallPolicyFor(verifier)` is the call policy; it does not admit a verifier session by itself.
