// Package goproxy queries Go module proxies for the latest version of a
// module, following the go command's GOPROXY rules.
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

	"golang.org/x/mod/module"
)

// Errors a lookup can end with. ErrNotFound means every proxy consulted
// answered 404 or 410 for the module path, which the proxy protocol defines
// as "not available on this proxy, but it may be found elsewhere". The
// other three mean the module could not be looked up at all, and callers
// report them as "not checked" rather than as failures.
var (
	ErrNotFound = errors.New("module not found on the proxy")
	ErrOff      = errors.New("version lookups are disabled by GOPROXY=off")
	ErrDirect   = errors.New("GOPROXY=direct names no module proxy to query")
	ErrPrivate  = errors.New("module matches GONOPROXY or GOPRIVATE and is not sent to a proxy")
)

// NotChecked reports whether err is one of the "could not look up" errors,
// with a short reason for display.
func NotChecked(err error) (reason string, ok bool) {
	switch {
	case errors.Is(err, ErrOff):
		return "GOPROXY=off", true
	case errors.Is(err, ErrDirect):
		return "GOPROXY=direct", true
	case errors.Is(err, ErrPrivate):
		return "matches GONOPROXY/GOPRIVATE", true
	}
	return "", false
}

// Source is one entry of a GOPROXY list: a proxy URL, or the keyword "off"
// or "direct". FallbackOnAnyError records that a "|" followed the entry,
// which the go command defines as "fall back to the next source after any
// error"; a "," means fall back only after a 404 or 410.
type Source struct {
	URL                string
	FallbackOnAnyError bool
}

const defaultGOPROXY = "https://proxy.golang.org,direct"

// Client resolves versions through a GOPROXY list.
type Client struct {
	Sources []Source
	NoProxy string // GONOPROXY (or GOPRIVATE when GONOPROXY is unset) glob list
	HTTP    *http.Client
}

// New builds a client from GOPROXY, GONOPROXY and GOPRIVATE in the
// environment, with the go command's defaults when they are unset.
func New() *Client {
	return FromEnv(os.Getenv("GOPROXY"), os.Getenv("GONOPROXY"), os.Getenv("GOPRIVATE"))
}

// FromEnv builds a client from explicit GOPROXY, GONOPROXY and GOPRIVATE
// values. GONOPROXY defaults to GOPRIVATE, as it does for the go command.
func FromEnv(goproxy, gonoproxy, goprivate string) *Client {
	noProxy := gonoproxy
	if noProxy == "" {
		noProxy = goprivate
	}
	return &Client{
		Sources: ParseGOPROXY(goproxy),
		NoProxy: noProxy,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ParseGOPROXY splits a GOPROXY value into sources, keeping track of
// whether each is followed by "," or "|". An empty value means the go
// command's default, "https://proxy.golang.org,direct".
func ParseGOPROXY(s string) []Source {
	s = strings.TrimSpace(s)
	if s == "" {
		s = defaultGOPROXY
	}
	var sources []Source
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != ',' && s[i] != '|' {
			continue
		}
		entry := strings.TrimSpace(s[start:i])
		if entry != "" {
			if entry != "off" && entry != "direct" {
				entry = strings.TrimRight(entry, "/")
			}
			sources = append(sources, Source{URL: entry, FallbackOnAnyError: i < len(s) && s[i] == '|'})
		}
		start = i + 1
	}
	return sources
}

// Info is a proxy's answer for @latest.
type Info struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// Latest returns the latest version of modPath, walking the GOPROXY list
// the way the go command does: each proxy is asked in turn, a 404 or 410
// moves on to the next source, and any other error moves on only when the
// entry was followed by "|". Reaching "off" or "direct" ends the walk:
// "direct" after a proxy's 404 is reported as ErrNotFound, since this tool
// does not consult version control; "direct" or "off" first is ErrDirect or
// ErrOff. Modules matching GONOPROXY/GOPRIVATE are never sent to a proxy.
//
// Resolution relies on the proxy's @latest endpoint, which the protocol
// marks optional but every mainstream proxy implements. Retractions are not
// applied.
func (c *Client) Latest(ctx context.Context, modPath string) (Info, error) {
	if c.NoProxy != "" && module.MatchPrefixPatterns(c.NoProxy, modPath) {
		return Info{}, fmt.Errorf("%s: %w", modPath, ErrPrivate)
	}
	escaped, err := module.EscapePath(modPath)
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", modPath, err)
	}
	sources := c.Sources
	if len(sources) == 0 {
		sources = ParseGOPROXY("")
	}
	var lastErr error
	for i, s := range sources {
		switch s.URL {
		case "off":
			return Info{}, fmt.Errorf("%s: %w", modPath, ErrOff)
		case "direct":
			if lastErr != nil {
				return Info{}, lastErr // a proxy said not found; direct is not consulted
			}
			return Info{}, fmt.Errorf("%s: %w", modPath, ErrDirect)
		}
		info, err := c.fetch(ctx, s.URL+"/"+escaped+"/@latest", modPath)
		if err == nil {
			return info, nil
		}
		lastErr = err
		if i == len(sources)-1 {
			break
		}
		if errors.Is(err, ErrNotFound) || s.FallbackOnAnyError {
			continue
		}
		return Info{}, err
	}
	return Info{}, lastErr
}

func (c *Client) fetch(ctx context.Context, url, modPath string) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Info{}, err
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
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
// letter becomes "!" followed by its lower-case form. An invalid module
// path is returned unchanged.
func EscapePath(p string) string {
	escaped, err := module.EscapePath(p)
	if err != nil {
		return p
	}
	return escaped
}

// maxMajorProbe bounds how many major paths above the current one
// NewerMajors will try, and missTolerance is how many consecutive missing
// majors it accepts before concluding there are no more. Go allows a
// project to skip a major number, so a single miss is not proof.
const (
	maxMajorProbe = 10
	missTolerance = 2
)

// MajorVersion is a newer major of a module, living at its own path.
type MajorVersion struct {
	Major   int    `json:"major"`
	Path    string `json:"path"`
	Version string `json:"version"` // latest within that major
}

// NewerMajors lists every newer major version of modPath, lowest first.
// Go puts the major version in the module path (".../v3", or ".v3" for
// gopkg.in), so a plain @latest query never sees one. This probes
// successive major paths above the module's own, stopping after
// missTolerance consecutive paths the proxy does not have.
func (c *Client) NewerMajors(ctx context.Context, modPath string) ([]MajorVersion, error) {
	base, cur := SplitMajor(modPath)
	var found []MajorVersion
	misses := 0
	for m := cur + 1; m <= cur+maxMajorProbe && misses < missTolerance; m++ {
		p := MajorPath(base, m)
		info, err := c.Latest(ctx, p)
		if errors.Is(err, ErrNotFound) {
			misses++
			continue
		}
		if err != nil {
			return nil, err
		}
		misses = 0
		found = append(found, MajorVersion{Major: m, Path: p, Version: info.Version})
	}
	return found, nil
}

// SplitMajor separates a module path from its major-version suffix using
// the go command's rules: "example.com/m/v3" gives ("example.com/m", 3),
// "gopkg.in/yaml.v3" gives ("gopkg.in/yaml", 3), and a path with no suffix
// is major 1 (which also covers v0 modules, since both live on the bare
// path). "/v1", "/v0" and suffixes with leading zeros are not suffixes.
func SplitMajor(modPath string) (base string, major int) {
	prefix, pathMajor, ok := module.SplitPathVersion(modPath)
	if !ok || pathMajor == "" {
		return modPath, 1
	}
	n, err := strconv.Atoi(pathMajor[2:]) // after "/v" or ".v"
	if err != nil || n < 1 {
		return prefix, 1
	}
	return prefix, n
}

// MajorPath is the inverse of SplitMajor. gopkg.in paths always carry a
// dot suffix, even at v1.
func MajorPath(base string, major int) string {
	if strings.HasPrefix(base, "gopkg.in/") {
		return base + ".v" + strconv.Itoa(major)
	}
	if major < 2 {
		return base
	}
	return base + "/v" + strconv.Itoa(major)
}
