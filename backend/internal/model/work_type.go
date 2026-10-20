package model

// IsSupportedWorkType lists the operations for which Engine has an executor.
func IsSupportedWorkType(workType string) bool {
	switch workType {
	case "analyze_sample", "inspect_sample_endpoint", "inspect_http_endpoint", "aggregate_summary", "run_security_check":
		return true
	default:
		return false
	}
}
