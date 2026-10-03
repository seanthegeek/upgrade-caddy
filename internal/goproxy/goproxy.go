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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/module"
	xsemver "golang.org/x/mod/semver"
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

	// ConfigErr, when set, is returned by every lookup: the GOPROXY value
	// was present but unusable, which the go command also refuses rather
	// than falling back to the default proxy.
	ConfigErr error
}

// New builds a client from the effective Go environment: GOPROXY,
// GONOPROXY and GOPRIVATE from the process environment, else from the file
// `go env -w` writes to (GOENV, or <user config dir>/go/env), as the go
// command resolves them. GOENV=off disables the file.
func New() *Client {
	return FromEnv(GoEnv("GOPROXY"), GoEnv("GONOPROXY"), GoEnv("GOPRIVATE"))
}

var (
	envFileOnce sync.Once
	envFile     map[string]string
)

// GoEnv returns the effective value of a Go environment variable: a
// non-empty process value wins, then the GOENV file, then "". A variable
// that is set but empty counts as unset, as in the go command's cfg.Getenv
// (cmd/go/internal/cfg/cfg.go), so `GOPRIVATE=` on the command line does
// not hide a private pattern persisted with `go env -w` and send those
// modules to the public proxy.
func GoEnv(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	envFileOnce.Do(func() { envFile = readGoEnvFile() })
	return envFile[key]
}

// readGoEnvFile parses the file `go env -w` maintains, KEY=VALUE per line,
// following the go command's location rules. As in the go command's own
// readEnvFile (cmd/go/internal/cfg), an unreadable file and malformed lines
// are ignored rather than treated as errors, so lookups here see the same
// environment `go get` would.
func readGoEnvFile() map[string]string {
	file := os.Getenv("GOENV")
	switch file {
	case "off":
		return nil
	case "":
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil
		}
		file = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	// Read whole and split by hand, as readEnvFile does: a scanner would
	// stop at its token limit and silently drop a long GOPRIVATE line that
	// the go command still honours. A line without "=" or not starting
	// with a capital letter (a comment, a blank, a corrupted line) is
	// ignored, also as the go command does.
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || k[0] < 'A' || 'Z' < k[0] {
			continue
		}
		vals[k] = v
	}
	return vals
}

// FromEnv builds a client from explicit GOPROXY, GONOPROXY and GOPRIVATE
// values. GONOPROXY defaults to GOPRIVATE, as it does for the go command.
func FromEnv(goproxy, gonoproxy, goprivate string) *Client {
	noProxy := gonoproxy
	if noProxy == "" {
		noProxy = goprivate
	}
	c := &Client{
		Sources: ParseGOPROXY(goproxy),
		NoProxy: noProxy,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	if goproxy != "" && len(c.Sources) == 0 {
		// Mirrors the go command: "GOPROXY list is not the empty string,
		// but contains no entries". Only the entries are trimmed, never the
		// whole value, so GOPROXY=" " is this error and not the default
		// public proxy (proxyList in cmd/go/internal/modfetch/proxy.go).
		c.ConfigErr = errors.New("GOPROXY is set but contains no entries")
	}
	return c
}

// ParseGOPROXY splits a GOPROXY value into sources, keeping track of
// whether each is followed by "," or "|". An empty value means the go
// command's default, "https://proxy.golang.org,direct"; a value that is
// only whitespace is not empty and yields no sources, as for the go
// command. As the go command does, an entry that looks like a host rather
// than a keyword or a URL ("proxy.example.com") gets "https://" prepended.
func ParseGOPROXY(s string) []Source {
	if s == "" {
		s = defaultGOPROXY
	}
	var sources []Source
	rest := s
	for {
		entry, sep, remainder := rest, "", ""
		if i := strings.IndexAny(rest, ",|"); i >= 0 {
			entry, sep, remainder = rest[:i], rest[i:i+1], rest[i+1:]
		}
		if entry = strings.TrimSpace(entry); entry != "" {
			if entry != "off" && entry != "direct" {
				// Mirrors cmd/go/internal/modfetch/proxy.go: single words are
				// keywords, anything with ":/" or an absolute path is a
				// complete URL, everything else is a host.
				if strings.ContainsAny(entry, ".:/") && !strings.Contains(entry, ":/") && !filepath.IsAbs(entry) && !path.IsAbs(entry) {
					entry = "https://" + entry
				}
				entry = strings.TrimRight(entry, "/")
			}
			sources = append(sources, Source{URL: entry, FallbackOnAnyError: sep == "|"})
		}
		if sep == "" {
			return sources
		}
		rest = remainder
	}
}

// Info is a proxy's answer for @latest.
type Info struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// Latest returns the latest version of modPath, walking the GOPROXY list
// the way the go command does: each proxy is asked in turn, a 404 or 410
// moves on to the next source, and any other error moves on only when the
// entry was followed by "|". Proxies may be http(s) URLs or file:// paths
// laid out like a proxy. Reaching "off" or "direct" ends the walk:
// "direct" after a proxy's 404 is reported as ErrNotFound, since this tool
// does not consult version control; "direct" reached any other way (first,
// or after a failure that a "|" let through) is ErrDirect, and "off" is
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
	if c.ConfigErr != nil {
		return Info{}, c.ConfigErr
	}
	if len(sources) == 0 {
		sources = ParseGOPROXY("")
	}
	var lastErr error
	for i, s := range sources {
		switch s.URL {
		case "off":
			return Info{}, fmt.Errorf("%s: %w", modPath, ErrOff)
		case "direct":
			// A proxy's 404/410 is final (direct is not consulted for a
			// module the proxy says it has no copy of); any other failure
			// that fell through a "|" lands here as "cannot look up".
			if errors.Is(lastErr, ErrNotFound) {
				return Info{}, lastErr
			}
			return Info{}, fmt.Errorf("%s: %w", modPath, ErrDirect)
		}
		var info Info
		if dir, isFile, perr := fileProxyDir(i, s.URL); perr != nil {
			err = perr
		} else if isFile {
			info, err = c.fetchFile(dir+"/"+escaped+"/@latest", modPath)
		} else if u, uerr := proxyURL(s.URL, escaped+"/@latest"); uerr != nil {
			err = uerr
		} else {
			info, err = c.fetch(ctx, u, modPath)
		}
		if err == nil {
			// A proxy answer that is not a canonical version compatible
			// with the module path is a broken proxy, not a version.
			// Without this a value like "garbage" would sort below every
			// real version and read as "current", and a short form like
			// "v1.2" (which module.Check accepts) would go into a plan
			// that go get resolves to "v1.2.0" and verify then rejects
			// after a long build. It is an error like any other, so a "|"
			// separator falls through to the next source.
			if cerr := module.Check(modPath, info.Version); cerr != nil {
				err = fmt.Errorf("%s: proxy %s returned an invalid version %q: %w", modPath, redacted(s.URL), info.Version, cerr)
			} else if c := module.CanonicalVersion(info.Version); c != info.Version {
				err = fmt.Errorf("%s: proxy %s returned an invalid version %q: not in canonical form (%s)", modPath, redacted(s.URL), info.Version, c)
			} else {
				return info, nil
			}
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

// proxyURL joins a proxy endpoint onto a GOPROXY base the way the go
// command does (newProxyRepo and getBody in cmd/go/internal/modfetch/
// proxy.go): the base is parsed and the endpoint appended to its path, so
// a base carrying a query string keeps it as a query instead of having the
// module path glued onto it. The base is referred to by position in errors,
// since url.Parse's error echoes its input, credentials included.
func proxyURL(base, endpoint string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("GOPROXY entry is not a valid URL")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + endpoint
	u.RawPath = ""
	return u.String(), nil
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		// Bytes that happen to form valid JSON are not an answer if the
		// connection failed before the body was complete.
		return Info{}, fmt.Errorf("%s: reading response: %w", redacted(url), err)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return Info{}, fmt.Errorf("%s: %w", modPath, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("%s: HTTP %d: %s", redacted(url), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return decodeInfo(body, redacted(url))
}

// redacted returns a URL with any password replaced by "xxxxx", for error
// messages: a GOPROXY entry may carry credentials, and errors end up on
// stderr and in JSON reports. (net/http already does this for its own
// transport errors.)
func redacted(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Redacted()
}

// fileProxyDir parses a proxy entry and, for a file:// URL, returns its
// decoded directory. As the go command requires, a file URL may carry
// nothing but a path: a host, query or fragment is an error. Errors name
// the entry by position, never by content: an unparseable entry cannot be
// redacted (url.Parse's own error echoes the input), and a parseable one
// is printed in redacted form.
func fileProxyDir(index int, entry string) (dir string, isFile bool, err error) {
	u, err := url.Parse(entry)
	if err != nil {
		return "", false, fmt.Errorf("GOPROXY entry %d is not a valid URL", index+1)
	}
	if u.Scheme != "file" {
		return "", false, nil
	}
	if *u != (url.URL{Scheme: u.Scheme, Path: u.Path, RawPath: u.RawPath}) {
		return "", false, fmt.Errorf("GOPROXY entry %d (%s): a file URL may contain only a path", index+1, u.Redacted())
	}
	return strings.TrimSuffix(filepath.FromSlash(u.Path), string(filepath.Separator)), true, nil
}

// fetchFile serves a file:// proxy, a directory laid out like a proxy. A
// missing file is the proxy protocol's 404.
func (c *Client) fetchFile(path, modPath string) (Info, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("%s: %w", modPath, ErrNotFound)
	}
	if err != nil {
		return Info{}, err
	}
	return decodeInfo(body, path)
}

func decodeInfo(body []byte, where string) (Info, error) {
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return Info{}, fmt.Errorf("%s: bad JSON: %w", where, err)
	}
	if info.Version == "" {
		return Info{}, fmt.Errorf("%s: empty version in response", where)
	}
	return info, nil
}

// Canonical returns v with a leading "v", as module versions are written.
func Canonical(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && v[0] != 'v' {
		return "v" + v
	}
	return v
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
// successive major paths above the current major, stopping after
// missTolerance consecutive paths the proxy does not have. That is a
// deliberate bound: a project that skips two consecutive major numbers
// would go unseen, in exchange for not sending ten requests per module on
// every check.
//
// The current major is the higher of the path's suffix and the installed
// version's major, because a bare path can hold v0, v1 or a
// vN+incompatible release: with v2.0.0+incompatible installed the probe
// starts at /v3, not /v2. installed may be "" when unknown.
func (c *Client) NewerMajors(ctx context.Context, modPath, installed string) ([]MajorVersion, error) {
	base, cur := SplitMajor(modPath)
	if m := xsemver.Major(Canonical(installed)); m != "" {
		if n, err := strconv.Atoi(m[1:]); err == nil && n > cur {
			cur = n
		}
	}
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
// path). "/v1", "/v0" and suffixes with leading zeros are not suffixes,
// but gopkg.in's ".v0" is one (module.SplitPathVersion accepts it) and
// means major 0, so that ".v1" still counts as a newer major.
func SplitMajor(modPath string) (base string, major int) {
	prefix, pathMajor, ok := module.SplitPathVersion(modPath)
	if !ok || pathMajor == "" {
		return modPath, 1
	}
	n, err := strconv.Atoi(pathMajor[2:]) // after "/v" or ".v"
	if err != nil || n < 0 {
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
