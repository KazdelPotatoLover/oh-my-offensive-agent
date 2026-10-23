package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeDefaultsExposeSettingsWithoutCredentials(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", `{"max_parallel_workers":5,"run_timeout_seconds":3600}`)
	t.Setenv("LLM_API_KEY", "test-secret-do-not-expose")
	t.Setenv("LLM_BASE_URL", "https://example.com/v1")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_API_FORMAT", "chat_completions")
	response := httptest.NewRecorder()
	RuntimeDefaults(response, httptest.NewRequest(http.MethodGet, "/api/runtime/defaults", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"max_parallel_workers":5`)
	require.Contains(t, response.Body.String(), `"model_configured":true`)
	require.NotContains(t, response.Body.String(), "test-secret")
	require.NotContains(t, response.Body.String(), "example.com")
	t.Setenv("LLM_API_KEY", "")
	response = httptest.NewRecorder()
	RuntimeDefaults(response, httptest.NewRequest(http.MethodGet, "/api/runtime/defaults", nil))
	require.Contains(t, response.Body.String(), `"model_configured":false`)
}
