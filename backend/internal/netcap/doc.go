// Package netcap is the production HTTP/HTTPS transport and capture gateway.
//
// It implements contract.TargetTransport and contract.CaptureGateway. Receipts
// are minted here after a real exchange or an authorized CONNECT. A model JSON
// object is not a receipt. The stub types refuse to mint evidence and must not
// be installed as a production fallback.
//
// ARTEX records worker traffic with an embedded mitmproxy and binds finding
// evidence to captured flow IDs. This package keeps that split between a
// recorded exchange and a later finding, and does not copy ARTEX's insecure
// upstream TLS flag or its fail-open passthrough.
package netcap
