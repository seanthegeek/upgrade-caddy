// Package goproxy queries a Go module proxy for the latest version of a module.
package goproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultProxy = "https://proxy.golang.org"

// Client talks to a single module proxy.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the first usable proxy in $GOPROXY, falling back
// to proxy.golang.org. "direct" and "off" entries are skipped.
func New() *Client {
	base := defaultProxy
	for _, p := range strings.FieldsFunc(os.Getenv("GOPROXY"), func(r rune) bool { return r == ',' || r == '|' }) {
		p = strings.TrimSpace(p)
		if p == "" || p == "direct" || p == "off" {
			continue
		}
		base = strings.TrimRight(p, "/")
		break
	}
	return &Client{BaseURL: base, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Info is the proxy's answer for @latest.
type Info struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// Latest returns the latest version of modPath known to the proxy. For
// untagged modules the proxy answers with a pseudo-version of the default
// branch head.
func (c *Client) Latest(ctx context.Context, modPath string) (Info, error) {
	url := c.BaseURL + "/" + EscapePath(modPath) + "/@latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return Info{}, fmt.Errorf("%s: bad JSON: %w", url, err)
	}
	if info.Version == "" {
		return Info{}, fmt.Errorf("%s: empty version in response", url)
	}
	return info, nil
}

// EscapePath applies the module proxy's case encoding: every upper-case
// letter becomes "!" followed by its lower-case form.
func EscapePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
