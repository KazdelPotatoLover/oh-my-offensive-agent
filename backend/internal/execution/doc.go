// Package execution runs worker commands in isolated containers.
//
// The API process implements contract.JobExecutor and speaks a fixed JSON
// protocol to a trusted broker. Only the broker opens the Docker socket, and
// it never evaluates a host shell. Execution containers are non-root, not
// privileged, and have no host network, host PID, or Docker socket.
//
// HTTP and HTTPS leave the container only through an internal network and an
// egress proxy. The proxy dials the capture callback, which calls the injected
// CaptureGateway. Setting HTTP_PROXY is not the isolation boundary.
//
// A non-zero exit code is an execution result. It does not fail the Run, and
// stdout is not an empirical source.
package execution
