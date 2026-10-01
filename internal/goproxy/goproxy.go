// Package goproxy queries a Go module proxy for the latest version of a module.
package goproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when the proxy has no such module. Proxies answer
// 404 or 410 for a module path that does not exist.
var ErrNotFound = errors.New("module not found")

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
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return Info{}, fmt.Errorf("%s: %w", modPath, ErrNotFound)
	}
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

// maxMajorProbe bounds how many successive major paths NewerMajor will try.
const maxMajorProbe = 10

// NewerMajor looks for a newer major version of modPath. Go puts the major
// version in the module path (".../v3"), so a plain @latest query never sees
// one. This probes the next major path and keeps going until the proxy says
// a path does not exist, returning the highest major found. ok is false when
// there is none. Modules that cannot use the /vN scheme, such as gopkg.in
// paths, are never probed.
func (c *Client) NewerMajor(ctx context.Context, modPath string) (path string, info Info, ok bool, err error) {
	if strings.HasPrefix(modPath, "gopkg.in/") {
		return "", Info{}, false, nil
	}
	base, cur := SplitMajor(modPath)
	for m := cur + 1; m <= cur+maxMajorProbe; m++ {
		p := MajorPath(base, m)
		i, err := c.Latest(ctx, p)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			return "", Info{}, false, err
		}
		path, info, ok = p, i, true
	}
	return path, info, ok, nil
}

// SplitMajor separates a module path from its major-version suffix.
// "example.com/m/v3" gives ("example.com/m", 3); a path with no suffix is
// major 1 (which also covers v0 modules, since both live on the bare path).
func SplitMajor(modPath string) (base string, major int) {
	i := strings.LastIndexByte(modPath, '/')
	if i < 0 {
		return modPath, 1
	}
	suffix := modPath[i+1:]
	if len(suffix) < 2 || suffix[0] != 'v' {
		return modPath, 1
	}
	n, err := strconv.Atoi(suffix[1:])
	if err != nil || n < 2 {
		return modPath, 1
	}
	return modPath[:i], n
}

// MajorPath is the inverse of SplitMajor.
func MajorPath(base string, major int) string {
	if major < 2 {
		return base
	}
	return base + "/v" + strconv.Itoa(major)
}
