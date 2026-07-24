package bootstrap

import "context"

// URLDiscoverer is the "url" bootstrap mode: a fixed discovery server URL.
type URLDiscoverer struct {
	BaseURL string
}

// BaseURLs returns the configured discovery server URL as the only candidate.
func (d *URLDiscoverer) BaseURLs(context.Context) ([]string, error) {
	return []string{d.BaseURL}, nil
}
