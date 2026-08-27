// Package directhttp constructs HTTP clients that do not consult proxy
// environment variables.
package directhttp

import (
	"net/http"
	"time"
)

func Transport() http.RoundTripper {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := base.Clone()
		transport.Proxy = nil
		return transport
	}
	return &http.Transport{}
}

func Client() *http.Client {
	return &http.Client{Transport: Transport()}
}

func ClientWithTimeout(timeout time.Duration) *http.Client {
	client := Client()
	client.Timeout = timeout
	return client
}
