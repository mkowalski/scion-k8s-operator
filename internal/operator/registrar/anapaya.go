package registrar

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotImplemented is returned by backends that are declared in the API
// but not yet implemented.
var ErrNotImplemented = errors.New("not implemented")

// Anapaya is a placeholder backend for registering SIGs through the
// Anapaya appliance API. A future implementation should use the OpenAPI
// client models shipped in Anapaya/ansible-collections
// (plugins/module_utils/appliance_api_client) to PATCH the appliance
// gateway configuration with the desired SIG set.
type Anapaya struct{}

// Ensure implements Backend and always fails: the anapaya backend is not
// yet available.
func (Anapaya) Ensure(context.Context, []SIG) error {
	return fmt.Errorf("anapaya backend: %w (use backend=http with the AS-side registrar service, or backend=manual and configure the appliance from status.registrar.desiredSIGs)", ErrNotImplemented)
}
