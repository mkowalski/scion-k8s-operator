package bootstrap

import "context"

// URLDiscoverer is the "url" bootstrap mode: a fixed discovery server URL.
type URLDiscoverer struct {
	BaseURL string
}

func (d *URLDiscoverer) BaseURLs(context.Context) ([]string, error) {
	return []string{d.BaseURL}, nil
}
