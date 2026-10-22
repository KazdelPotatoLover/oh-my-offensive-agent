package provider

import (
	"os"
	"strconv"
	"strings"
)

// ProcessModelSettings is the server-wide model admission policy. It is not
// the worker pool size. A non-positive in-flight limit means unlimited.
// RecoveryAttempts <= 1 leaves the session at one Provider.Call.
type ProcessModelSettings struct {
	MaxInFlight      int
	MaxOutputTokens  int
	RecoveryAttempts int
}

// LoadProcessModelSettings reads LLM_MAX_INFLIGHT, LLM_MAX_OUTPUT_TOKENS, and
// LLM_MODEL_RECOVERY_ATTEMPTS. An empty value keeps the zero default. A value
// above the output hard cap is rejected instead of being clamped.
func LoadProcessModelSettings() (ProcessModelSettings, error) {
	var settings ProcessModelSettings
	inFlight, err := optionalNonNegative("LLM_MAX_INFLIGHT")
	if err != nil {
		return settings, err
	}
	output, err := optionalNonNegative("LLM_MAX_OUTPUT_TOKENS")
	if err != nil {
		return settings, err
	}
	if output > HardMaxOutputTokens {
		return settings, &CallError{
			Class:     ClassOutputLimit,
			Retryable: false,
			Summary:   ErrModelConfigInvalid.Error() + ": LLM_MAX_OUTPUT_TOKENS exceeds 8192",
			unwrap:    []error{ErrModelConfigInvalid},
		}
	}
	attempts, err := optionalNonNegative("LLM_MODEL_RECOVERY_ATTEMPTS")
	if err != nil {
		return settings, err
	}
	if attempts > 8 {
		return settings, &CallError{
			Class:     ClassOutputLimit,
			Retryable: false,
			Summary:   ErrModelConfigInvalid.Error() + ": LLM_MODEL_RECOVERY_ATTEMPTS must be 0..8",
			unwrap:    []error{ErrModelConfigInvalid},
		}
	}
	settings.MaxInFlight = inFlight
	settings.MaxOutputTokens = output
	settings.RecoveryAttempts = attempts
	return settings, nil
}

// SessionAdmissionFromEnv builds the shared upstream cap for one start.
// A non-positive in-flight limit still returns a limiter so calls are counted.
// Recovery stays at one Provider.Call unless attempts are explicitly above 1.
func SessionAdmissionFromEnv() (*RequestLimiter, int, RecoveryPolicy, error) {
	settings, err := LoadProcessModelSettings()
	if err != nil {
		return nil, 0, RecoveryPolicy{}, err
	}
	recovery := RecoveryPolicy{}
	if settings.RecoveryAttempts > 1 {
		recovery = RecoveryPolicy{MaxAttempts: settings.RecoveryAttempts}
	}
	return NewRequestLimiter(settings.MaxInFlight), settings.MaxOutputTokens, recovery, nil
}

func optionalNonNegative(name string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, &CallError{
			Class:     ClassOutputLimit,
			Retryable: false,
			Summary:   ErrModelConfigInvalid.Error() + ": " + name + " must be a non-negative integer",
			unwrap:    []error{ErrModelConfigInvalid},
		}
	}
	return value, nil
}
