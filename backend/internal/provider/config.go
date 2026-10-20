package provider

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	FormatChatCompletions = "chat_completions"
	FormatResponses       = "responses"
)

var ErrModelConfigInvalid = errors.New("invalid model configuration")

type Config struct {
	APIFormat string
	BaseURL   string
	APIKey    string
	Model     string
}

func ConfigFromEnv() Config {
	return Config{APIFormat: os.Getenv("LLM_API_FORMAT"), BaseURL: os.Getenv("LLM_BASE_URL"), APIKey: os.Getenv("LLM_API_KEY"), Model: os.Getenv("LLM_MODEL")}
}

func (c Config) Normalized() (Config, error) {
	c.APIFormat = strings.TrimSpace(c.APIFormat)
	if c.APIFormat == "" {
		c.APIFormat = FormatChatCompletions
	}
	if c.APIFormat != FormatChatCompletions && c.APIFormat != FormatResponses {
		return c, fmt.Errorf("%w: LLM_API_FORMAT must be chat_completions or responses", ErrModelConfigInvalid)
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	if strings.TrimSpace(c.APIKey) == "" || c.BaseURL == "" || c.Model == "" {
		return c, ErrModelConfigMissing
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return c, fmt.Errorf("%w: LLM_BASE_URL must be an HTTP(S) API root without credentials, query, or fragment", ErrModelConfigInvalid)
	}
	if strings.HasSuffix(u.Path, "/responses") || strings.HasSuffix(u.Path, "/chat/completions") {
		return c, fmt.Errorf("%w: LLM_BASE_URL must be the API root (for example https://api.openai.com/v1)", ErrModelConfigInvalid)
	}
	return c, nil
}

func NewConfiguredProvider(c Config, client *http.Client) (Provider, error) {
	c, err := c.Normalized()
	if err != nil {
		return nil, err
	}
	if c.APIFormat == FormatResponses {
		return NewResponsesProvider(c.BaseURL, c.APIKey, c.Model, client), nil
	}
	return NewOpenAIProvider(c.BaseURL, c.APIKey, c.Model, client), nil
}

// FromSnapshot freezes routing to the configuration recorded when the run was created.
// Secrets are intentionally read from the environment and never stored in the snapshot.
func FromSnapshot(snapshot map[string]interface{}, defaultScenario MockScenario) (Provider, error) {
	typ, _ := snapshot["provider"].(string)
	if typ == "" || typ == "mock" {
		scenario, _ := snapshot["mock_scenario"].(string)
		if scenario == "" {
			scenario = string(defaultScenario)
		}
		return NewMockProvider(MockScenario(scenario)), nil
	}
	if typ != "openai" && typ != "remote" {
		return nil, fmt.Errorf("%w: unsupported provider", ErrModelConfigInvalid)
	}
	c := ConfigFromEnv()
	// Old runs were created with the Chat Completions adapter.
	c.APIFormat = FormatChatCompletions
	if f, ok := snapshot["api_format"].(string); ok && f != "" {
		c.APIFormat = f
	}
	if endpoint, ok := snapshot["endpoint"].(string); ok && endpoint != "" {
		c.BaseURL = endpoint
	}
	if m, ok := snapshot["model"].(string); ok && m != "" {
		c.Model = m
	}
	return NewConfiguredProvider(c, nil)
}
