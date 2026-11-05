package auth

import (
	"fmt"
	"net/http"
	"time"
)

// proxiedHTTPClient builds a 30 second HTTP client whose transport honours the given proxy URL.
func proxiedHTTPClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := ConfigureTransportProxy(transport, proxyURL, nil); err != nil {
		return nil, fmt.Errorf("proxy configuration failed: %w", err)
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}
