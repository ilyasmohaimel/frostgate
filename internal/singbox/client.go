package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	Controller string
	Secret     string
	HTTP       *http.Client
}

func New(controller, secret string) *Client {
	trimmed := controller
	if trimmed == "" {
		trimmed = "http://127.0.0.1:9090"
	}
	return &Client{
		Controller: trimmed,
		Secret:     secret,
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type delayResponse struct {
	Delay uint16 `json:"delay"`
}

type proxiesResponse struct {
	Proxies map[string]json.RawMessage `json:"proxies"`
}

func (c *Client) authHeader(req *http.Request) {
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
}

func (c *Client) GetProxies(ctx context.Context) (map[string]json.RawMessage, error) {
	u := c.Controller + "/proxies"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("singbox controller unreachable: %w", categorizeErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("singbox-api: unauthorized (bad secret)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("singbox-api: unexpected status %d", resp.StatusCode)
	}
	var pr proxiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("singbox-api: malformed proxies response: %w", err)
	}
	return pr.Proxies, nil
}

func (c *Client) CheckOutbound(ctx context.Context, tag string) error {
	proxies, err := c.GetProxies(ctx)
	if err != nil {
		return err
	}
	if _, ok := proxies[tag]; !ok {
		return fmt.Errorf("outbound-not-found: %q not in controller proxies", tag)
	}
	return nil
}

func (c *Client) Delay(ctx context.Context, outboundTag, testURL string, timeout time.Duration) (time.Duration, error) {
	if testURL == "" {
		testURL = "https://www.gstatic.com/generate_204"
	}
	escapedTag := url.PathEscape(outboundTag)
	u := fmt.Sprintf("%s/proxies/%s/delay?url=%s&timeout=%d", c.Controller, escapedTag, url.QueryEscape(testURL), timeout.Milliseconds())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	c.authHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("singbox-api: controller unreachable: %w", categorizeErr(err))
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var dr delayResponse
		if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
			return 0, fmt.Errorf("singbox-api: malformed delay response: %w", err)
		}
		if dr.Delay == 0 {
			return 0, fmt.Errorf("proxy-test: zero delay from controller")
		}
		return time.Duration(dr.Delay) * time.Millisecond, nil
	case http.StatusUnauthorized:
		return 0, fmt.Errorf("singbox-api: unauthorized")
	case http.StatusNotFound:
		return 0, fmt.Errorf("outbound-not-found: %q", outboundTag)
	case http.StatusServiceUnavailable:
		return 0, fmt.Errorf("proxy-test: outbound %q failed url test", outboundTag)
	case http.StatusGatewayTimeout:
		return 0, fmt.Errorf("timeout: outbound %q delay test timed out", outboundTag)
	default:
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return 0, fmt.Errorf("singbox-api: status %d", resp.StatusCode)
	}
}

func categorizeErr(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	switch {
	case contains(s, "connection refused"):
		return fmt.Errorf("connection-refused: %w", err)
	case contains(s, "no such host"):
		return fmt.Errorf("dns: %w", err)
	case contains(s, "timeout"), contains(s, "deadline"):
		return fmt.Errorf("timeout: %w", err)
	default:
		return err
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// Parse helpers exported for tests
func ParseDelay(b []byte) (time.Duration, error) {
	var dr delayResponse
	if err := json.Unmarshal(b, &dr); err != nil {
		return 0, err
	}
	return time.Duration(dr.Delay) * time.Millisecond, nil
}

var _ = strconv.Itoa
