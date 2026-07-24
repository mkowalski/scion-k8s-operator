// Package registrar syncs the cluster's node SIG set to the AS side.
package registrar

import "context"

// SIG describes one node's SIG endpoints as registered on the AS side.
// The JSON shape matches the AS-side registrar service body
// (map[nodeName]{"ctrl_addr","data_addr"}); Name is the map key.
type SIG struct {
	Name     string `json:"-"`
	CtrlAddr string `json:"ctrl_addr"`
	DataAddr string `json:"data_addr"`
}

// Backend reconciles the full desired SIG set on the AS side.
type Backend interface {
	Ensure(ctx context.Context, sigs []SIG) error
}

// Manual is a no-op backend; the desired set is only published in status.
type Manual struct{}

// Ensure implements Backend and does nothing.
func (Manual) Ensure(context.Context, []SIG) error { return nil }
