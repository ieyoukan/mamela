package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// Path is a parsed path template: literal segments and "{id}" segments.
type Path []Segment

// Segment is one path segment: a literal, or an ID when ID is set.
type Segment struct {
	Literal string
	ID      string
}

// ParsePath parses a path template such as "/items/{item}", or "/" (a query API's one endpoint,
// whose calls fixed parameters tell apart). Every segment is non-empty; an ID segment is the whole
// segment; an ID appears once.
func ParsePath(tmpl string) (Path, error) {
	if tmpl == "/" {
		return Path{}, nil
	}
	rest, ok := strings.CutPrefix(tmpl, "/")
	if !ok || rest == "" {
		return nil, errors.New("a path starts with / and has a segment, or is /")
	}
	var p Path
	seen := map[string]bool{}
	for s := range strings.SplitSeq(rest, "/") {
		if id, ok := strings.CutPrefix(s, "{"); ok {
			id, ok = strings.CutSuffix(id, "}")
			if !ok || !validName(id) || seen[id] {
				return nil, fmt.Errorf("segment %q: an ID is {name}, once per path", s)
			}
			seen[id] = true
			p = append(p, Segment{ID: id})
			continue
		}
		if err := checkSegment(s); err != nil {
			return nil, err
		}
		p = append(p, Segment{Literal: s})
	}
	return p, nil
}

// Match matches a concrete path against the template and returns the IDs.
func (p Path) Match(path string) (map[string]string, bool) {
	if len(p) == 0 {
		return map[string]string{}, path == "/"
	}
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return nil, false
	}
	segs := strings.Split(rest, "/")
	if len(segs) != len(p) {
		return nil, false
	}
	ids := map[string]string{}
	for i, s := range segs {
		if checkSegment(s) != nil {
			return nil, false
		}
		if p[i].ID != "" {
			ids[p[i].ID] = s
		} else if p[i].Literal != s {
			return nil, false
		}
	}
	return ids, true
}

// IDs returns the names of the ID segments, in order.
func (p Path) IDs() []string {
	var out []string
	for _, s := range p {
		if s.ID != "" {
			out = append(out, s.ID)
		}
	}
	return out
}

// CheckPath checks a concrete path: segments of letters, digits and ._~:@=+,!- only (a Proxmox
// task ID is "UPID:node:...:user@realm!token:"), none of
// them "." or "..". Escapes, queries and fragments are not paths.
func CheckPath(path string) error {
	if path == "/" {
		return nil
	}
	rest, ok := strings.CutPrefix(path, "/")
	if !ok || rest == "" {
		return errors.New("a path starts with / and has a segment, or is /")
	}
	for s := range strings.SplitSeq(rest, "/") {
		if err := checkSegment(s); err != nil {
			return err
		}
	}
	return nil
}

func checkSegment(s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("path segment %q", s)
	}
	for _, c := range s {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.ContainsRune("._~:@=+,!-", c)) {
			return fmt.Errorf("path segment %q: character %q", s, c)
		}
	}
	return nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		// "." for indexed names of query APIs ("Filter.1.Name", "InstanceId.1").
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
