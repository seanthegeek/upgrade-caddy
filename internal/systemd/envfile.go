package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// EnvFile is one EnvironmentFile= setting of a unit.
type EnvFile struct {
	Path     string `json:"path"`               // may contain shell globs, as systemd allows
	Optional bool   `json:"optional,omitempty"` // "-" prefix: a file that cannot be found is skipped
}

// LoadEnvironment composes the variables a unit's processes get from its own
// settings, in the order systemd merges them (strv_env_merge in
// src/core/exec-invoke.c, v255, later entries winning): Environment= first,
// then every EnvironmentFile= in order, each file's later assignments
// overriding earlier ones. An optional ("-") file is skipped on any
// failure, a relative path, no match for its pattern, an unreadable file
// or one that does not parse, exactly as exec_context_load_environment in
// src/core/execute.c continues past every error of an "ignore" entry; for
// a required file each of those is an error, as it fails the unit. The
// manager's own environment, the login variables for User=,
// PassEnvironment= and UnsetEnvironment= are not part of this list.
func LoadEnvironment(u Unit) ([]string, error) {
	env := MergeEnv(nil, u.Environment)
	for _, f := range u.EnvironmentFiles {
		vars, err := loadEnvFile(f.Path)
		if err != nil {
			if f.Optional {
				continue
			}
			return nil, fmt.Errorf("EnvironmentFile=%s: %w", f.Path, err)
		}
		env = MergeEnv(env, vars)
	}
	return env, nil
}

// loadEnvFile reads every file an EnvironmentFile= pattern matches, merged
// in order.
func loadEnvFile(pattern string) ([]string, error) {
	if !filepath.IsAbs(pattern) {
		return nil, errors.New("path is not absolute")
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, os.ErrNotExist
	}
	var env []string
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		vars, err := ParseEnvFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		env = MergeEnv(env, vars)
	}
	return env, nil
}

// MergeEnv adds KEY=VALUE entries to env, replacing an existing KEY in place
// and appending new ones, as systemd's strv_env_replace does. It returns
// env, grown as needed.
func MergeEnv(env, add []string) []string {
	for _, kv := range add {
		k, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, have := range env {
			if hk, _, _ := strings.Cut(have, "="); hk == k {
				env[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, kv)
		}
	}
	return env
}

// ParseEnvFile parses an EnvironmentFile= the way systemd does
// (parse_env_file_internal in src/basic/env-file.c, v255): lines of
// KEY=VALUE, "#" and ";" comments, unquoted values with shell backslash
// escapes and line continuation, single-quoted values taken verbatim,
// double-quoted values with the shell's escapes for \" \\ \` and \$, and
// leading and trailing whitespace outside quotes dropped. A later
// assignment of the same key wins. Invalid UTF-8 or a unicode noncharacter
// anywhere is an error, as it is for systemd.
func ParseEnvFile(data []byte) ([]string, error) {
	const (
		preKey = iota
		key
		preValue
		value
		valueEscape
		singleQuote
		doubleQuote
		doubleQuoteEscape
		comment
		commentEscape
	)
	const (
		whitespace = " \t\n\r"
		newline    = "\n\r"
		comments   = "#;"
		needEscape = "\"\\`$"
	)
	var (
		out            []string
		k, v           []byte
		lastKeySpace   = -1
		lastValueSpace = -1
		state          = preKey
		trimKey        = func() string {
			if lastKeySpace >= 0 {
				return string(k[:lastKeySpace])
			}
			return string(k)
		}
		trimValue = func() string {
			if lastValueSpace >= 0 {
				return string(v[:lastValueSpace])
			}
			return string(v)
		}
		push = func(key, val string) error {
			if !validUTF8(key) || !validUTF8(val) {
				return errors.New("invalid UTF-8 in environment file")
			}
			out = MergeEnv(out, []string{key + "=" + val})
			k, v = k[:0], nil
			lastValueSpace = -1
			return nil
		}
	)
	for _, c := range data {
		switch state {
		case preKey:
			if strings.IndexByte(comments, c) >= 0 {
				state = comment
			} else if strings.IndexByte(whitespace, c) < 0 {
				state = key
				lastKeySpace = -1
				k = append(k, c)
			}
		case key:
			if strings.IndexByte(newline, c) >= 0 {
				state = preKey
				k = k[:0]
			} else if c == '=' {
				state = preValue
				lastValueSpace = -1
			} else {
				if strings.IndexByte(whitespace, c) < 0 {
					lastKeySpace = -1
				} else if lastKeySpace < 0 {
					lastKeySpace = len(k)
				}
				k = append(k, c)
			}
		case preValue:
			if strings.IndexByte(newline, c) >= 0 {
				state = preKey
				if err := push(trimKey(), string(v)); err != nil {
					return nil, err
				}
			} else if c == '\'' {
				state = singleQuote
			} else if c == '"' {
				state = doubleQuote
			} else if c == '\\' {
				state = valueEscape
			} else if strings.IndexByte(whitespace, c) < 0 {
				state = value
				v = append(v, c)
			}
		case value:
			if strings.IndexByte(newline, c) >= 0 {
				state = preKey
				if err := push(trimKey(), trimValue()); err != nil {
					return nil, err
				}
			} else if c == '\\' {
				state = valueEscape
				lastValueSpace = -1
			} else {
				if strings.IndexByte(whitespace, c) < 0 {
					lastValueSpace = -1
				} else if lastValueSpace < 0 {
					lastValueSpace = len(v)
				}
				v = append(v, c)
			}
		case valueEscape:
			state = value
			if strings.IndexByte(newline, c) < 0 { // an escaped newline is eaten entirely
				v = append(v, c)
			}
		case singleQuote:
			if c == '\'' {
				state = preValue
			} else {
				v = append(v, c)
			}
		case doubleQuote:
			if c == '"' {
				state = preValue
			} else if c == '\\' {
				state = doubleQuoteEscape
			} else {
				v = append(v, c)
			}
		case doubleQuoteEscape:
			state = doubleQuote
			if strings.IndexByte(needEscape, c) >= 0 {
				v = append(v, c)
			} else if c != '\n' {
				v = append(v, '\\', c) // the shell keeps the backslash for other characters
			}
		case comment:
			if c == '\\' {
				state = commentEscape
			} else if strings.IndexByte(newline, c) >= 0 {
				state = preKey
			}
		case commentEscape:
			if strings.IndexByte(newline, c) >= 0 {
				state = preKey
			} else {
				state = comment
			}
		}
	}
	switch state {
	case preValue, valueEscape, singleQuote, doubleQuote, doubleQuoteEscape:
		if err := push(trimKey(), string(v)); err != nil {
			return nil, err
		}
	case value:
		if err := push(trimKey(), trimValue()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validUTF8 is systemd's utf8_is_valid: well-formed UTF-8 with no unicode
// noncharacter (U+FDD0..U+FDEF and the last two code points of every
// plane) and no surrogate.
func validUTF8(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if (r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE {
			return false
		}
	}
	return true
}

// environmentFromBus reads the unit's Environment= over D-Bus, which keeps
// each assignment whole; systemctl show joins them with spaces, so a value
// containing a space cannot be recovered from it.
func environmentFromBus(ctx context.Context, unit string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "busctl", "--json=short", "get-property",
		"org.freedesktop.systemd1", unitObjectPath(unit), "org.freedesktop.systemd1.Service", "Environment").Output()
	if err != nil {
		return nil, err
	}
	return parseBusStrings(out)
}

// environmentFilesFromBus reads the unit's EnvironmentFiles= over D-Bus, an
// array of (path, ignore-errors) pairs.
func environmentFilesFromBus(ctx context.Context, unit string) ([]EnvFile, error) {
	out, err := exec.CommandContext(ctx, "busctl", "--json=short", "get-property",
		"org.freedesktop.systemd1", unitObjectPath(unit), "org.freedesktop.systemd1.Service", "EnvironmentFiles").Output()
	if err != nil {
		return nil, err
	}
	return parseBusEnvFiles(out)
}

// parseBusEnvFiles decodes busctl's JSON for the "a(sb)" EnvironmentFiles
// property: {"type":"a(sb)","data":[["/etc/caddy/env",false]]}.
func parseBusEnvFiles(data []byte) ([]EnvFile, error) {
	var prop struct {
		Data [][2]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &prop); err != nil {
		return nil, fmt.Errorf("busctl EnvironmentFiles: %w", err)
	}
	var files []EnvFile
	for _, pair := range prop.Data {
		var f EnvFile
		if err := json.Unmarshal(pair[0], &f.Path); err != nil {
			return nil, fmt.Errorf("busctl EnvironmentFiles: %w", err)
		}
		if err := json.Unmarshal(pair[1], &f.Optional); err != nil {
			return nil, fmt.Errorf("busctl EnvironmentFiles: %w", err)
		}
		files = append(files, f)
	}
	return files, nil
}

// envFileShowRe matches systemctl show's rendering of one EnvironmentFiles=
// entry, "%s (ignore_errors=%s)" in src/systemctl/systemctl-show.c (v255).
var envFileShowRe = regexp.MustCompile(`^(.*) \(ignore_errors=(yes|no)\)$`)

// parseEnvFileShow parses one EnvironmentFiles= line of systemctl show.
func parseEnvFileShow(v string) (EnvFile, bool) {
	m := envFileShowRe.FindStringSubmatch(v)
	if m == nil {
		return EnvFile{}, false
	}
	return EnvFile{Path: m[1], Optional: m[2] == "yes"}, true
}
