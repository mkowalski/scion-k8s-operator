//go:build tools

// Package tools pins module dependencies that have no direct imports yet,
// so `go mod tidy` does not strip them.
package tools

import (
	_ "github.com/scionproto/scion/gateway"
	_ "k8s.io/client-go/kubernetes"
	_ "sigs.k8s.io/controller-runtime"
)
