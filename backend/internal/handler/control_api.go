package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/controlplane"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ControlAPI serves the shared command, main-message, and workbench routes.
// Mount it inside the existing /api group. These routes use the contract
// envelope. They do not replace the legacy string-error routes.
type ControlAPI struct {
	Controller contract.ExecutionController
	Workbench  contract.WorkbenchQuery
	Main       *controlplane.MainAgent
}

func NewControlAPI(controller contract.ExecutionController, workbench contract.WorkbenchQuery, main *controlplane.MainAgent) *ControlAPI {
	return &ControlAPI{Controller: controller, Workbench: workbench, Main: main}
}

// MountControlAPI registers paths relative to the caller. The caller must
// already be inside /api, otherwise the public paths gain an extra /api prefix.
func MountControlAPI(r chi.Router, api *ControlAPI) {
	r.Post("/runs/{runID}/commands", api.PostCommand)
	r.Get("/runs/{runID}/workbench", api.GetWorkbench)
	r.Post("/runs/{runID}/main/messages", api.PostMainMessage)
	r.Get("/runs/{runID}/main/messages", api.GetMainMessages)
}

func (api *ControlAPI) PostCommand(w http.ResponseWriter, r *http.Request) {
	runID, ok := runIDFromRequest(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var cmd contract.Command
	if !decodeBody(w, r, &cmd) {
		return
	}
	if api == nil || api.Controller == nil {
		writeContractError(w, persistenceUnavailable())
		return
	}
	result, err := api.Controller.Apply(r.Context(), humanPrincipal(r, runID), humanScope(runID, cmd.ExpectedEpoch), key, cmd)
	if err != nil {
		writeContractError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

func (api *ControlAPI) PostMainMessage(w http.ResponseWriter, r *http.Request) {
	runID, ok := runIDFromRequest(w, r)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body contract.MainMessageSubmit
	if !decodeBody(w, r, &body) {
		return
	}
	if api == nil || api.Main == nil {
		writeContractError(w, persistenceUnavailable())
		return
	}
	accepted, err := api.Main.Submit(r.Context(), humanPrincipal(r, runID), key, body)
	if err != nil {
		writeContractError(w, err)
		return
	}
	// 202 records the turn. It does not mean a control action has finished.
	respondJSON(w, http.StatusAccepted, accepted)
}

func (api *ControlAPI) GetWorkbench(w http.ResponseWriter, r *http.Request) {
	runID, ok := runIDFromRequest(w, r)
	if !ok {
		return
	}
	after, limit, ok := pageQuery(w, r, "after_event_seq")
	if !ok {
		return
	}
	if api == nil || api.Workbench == nil {
		writeContractError(w, persistenceUnavailable())
		return
	}
	snap, err := api.Workbench.Snapshot(r.Context(), runID, contract.WorkbenchQueryParams{AfterEventSeq: after, Limit: limit})
	if err != nil {
		writeContractError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, snap)
}

func (api *ControlAPI) GetMainMessages(w http.ResponseWriter, r *http.Request) {
	runID, ok := runIDFromRequest(w, r)
	if !ok {
		return
	}
	after, limit, ok := pageQuery(w, r, "after_seq")
	if !ok {
		return
	}
	if api == nil || api.Workbench == nil {
		writeContractError(w, persistenceUnavailable())
		return
	}
	page, err := api.Workbench.MainMessages(r.Context(), runID, after, limit)
	if err != nil {
		writeContractError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, page)
}

func runIDFromRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeContractError(w, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: "run id must be a UUID", SuggestedAction: contract.ActionNone})
		return uuid.Nil, false
	}
	return id, true
}

func humanPrincipal(r *http.Request, runID uuid.UUID) contract.Principal {
	actor := strings.TrimSpace(r.Header.Get("X-Actor-Id"))
	if actor == "" {
		actor = "operator"
	}
	return contract.Principal{
		Kind:                contract.PrincipalHuman,
		ActorID:             actor,
		RunID:               runID,
		ExplicitUserCommand: true,
	}
}

func humanScope(runID uuid.UUID, epoch int) contract.ExecutionScope {
	return contract.ExecutionScope{RunID: runID, EpochNumber: epoch, Role: contract.RoleMainAgent}
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(contract.HeaderIdempotencyKey)
	if err := contract.RequireIdempotencyKey(key); err != nil {
		writeContractError(w, err)
		return "", false
	}
	return strings.TrimSpace(key), true
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeContractError(w, &contract.Error{Code: contract.CodePayloadTooLarge, Scope: contract.ScopeRequest, Message: "request body exceeds 1MB", SuggestedAction: contract.ActionNone})
			return false
		}
		writeContractError(w, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: "request body is not valid JSON", SuggestedAction: contract.ActionNone})
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeContractError(w, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: "request body must contain one JSON object", SuggestedAction: contract.ActionNone})
		return false
	}
	return true
}

func pageQuery(w http.ResponseWriter, r *http.Request, afterName string) (int64, int, bool) {
	after := int64(0)
	if raw := r.URL.Query().Get(afterName); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeContractError(w, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: afterName + " must be a non-negative integer", SuggestedAction: contract.ActionNone})
			return 0, 0, false
		}
		after = n
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeContractError(w, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: "limit must be a non-negative integer", SuggestedAction: contract.ActionNone})
			return 0, 0, false
		}
		limit = n
	}
	return after, limit, true
}

func writeContractError(w http.ResponseWriter, err error) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		ce = persistenceUnavailable()
	}
	respondJSON(w, ce.Status(), ce.Envelope())
}

func persistenceUnavailable() *contract.Error {
	return &contract.Error{Code: contract.CodePersistenceError, Scope: contract.ScopeRequest, Message: "control API is not configured", SuggestedAction: contract.ActionNone}
}
