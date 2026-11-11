package protocol

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var (
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	artifactID   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	placeholder  = regexp.MustCompile(`\{artifact:([a-z][a-z0-9-]{0,31})\.(name|sha256|base64)\}`)
	// contentRef is a reference to an artifact's content, which only the host fills, when it sends.
	contentRef = regexp.MustCompile(`\{artifact:([a-z][a-z0-9-]{0,31})\.base64\}`)
)

// CheckArtifacts verifies a change's artifacts, and that its requests refer only to them: an
// upload names one, and a placeholder names one and one of its facts.
func CheckArtifacts(c Change) error {
	ids := map[string]Artifact{}
	for _, a := range c.Artifacts {
		if !artifactID.MatchString(a.ID) {
			return fmt.Errorf("artifact ID %q", a.ID)
		}
		if _, dup := ids[a.ID]; dup {
			return fmt.Errorf("artifact %s is declared twice", a.ID)
		}
		ids[a.ID] = a
		switch a.Kind {
		case ArtifactDownload:
			u, err := url.Parse(a.URL)
			switch {
			case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "":
				return fmt.Errorf("artifact %s: %q is not an https URL", a.ID, a.URL)
			case !sha256Hex.MatchString(a.SHA256):
				return fmt.Errorf("artifact %s: sha256 %q is not 64 lowercase hex digits", a.ID, a.SHA256)
			case !artifactName.MatchString(a.Name):
				return fmt.Errorf("artifact %s: name %q is not a plain file name", a.ID, a.Name)
			case a.Node != "" || a.NodeID != 0 || a.Network != nil:
				return fmt.Errorf("artifact %s: a download has no node or network", a.ID)
			}
		case ArtifactSeed:
			switch {
			case a.Node == "" || a.NodeID < 0 || a.Network == nil:
				return fmt.Errorf("artifact %s: a seed needs a node, a network, and its ID or 0 (Mamela assigns one)", a.ID)
			case a.Format != "" && a.Format != SeedISO && a.Format != SeedUserData:
				return fmt.Errorf("artifact %s: seed format %q (iso or user-data)", a.ID, a.Format)
			case a.URL != "" || a.SHA256 != "" || a.Name != "":
				return fmt.Errorf("artifact %s: a seed's name and sha256 are the host's, not the change's", a.ID)
			case a.Network.Mode != "dhcp" && a.Network.Mode != "static":
				return fmt.Errorf("artifact %s: network mode %q", a.ID, a.Network.Mode)
			}
		default:
			return fmt.Errorf("artifact %s: unknown kind %q", a.ID, a.Kind)
		}
	}
	for _, r := range c.Requests {
		if r.Upload != "" {
			if _, ok := ids[r.Upload]; !ok {
				return fmt.Errorf("request %s %s uploads %q, which is not an artifact of the change", r.Method, r.Path, r.Upload)
			}
		}
		for name, vals := range r.Params {
			for _, v := range vals {
				for _, m := range placeholder.FindAllStringSubmatch(v, -1) {
					if _, ok := ids[m[1]]; !ok {
						return fmt.Errorf("request %s %s: parameter %s names artifact %q, which is not one of the change", r.Method, r.Path, name, m[1])
					}
				}
				if strings.Contains(placeholder.ReplaceAllString(v, ""), "{artifact:") {
					return fmt.Errorf("request %s %s: parameter %s has a malformed artifact reference", r.Method, r.Path, name)
				}
			}
		}
	}
	return nil
}

// FillArtifacts replaces the artifact references in params with the facts of ready artifacts. It
// fails when one names an artifact that is not ready.
func FillArtifacts(params map[string][]string, ready map[string]ArtifactInfo) (map[string][]string, error) {
	out := make(map[string][]string, len(params))
	for _, k := range slices.Sorted(maps.Keys(params)) {
		for _, v := range params[k] {
			var missing string
			filled := placeholder.ReplaceAllStringFunc(v, func(ref string) string {
				m := placeholder.FindStringSubmatch(ref)
				if m[2] == "base64" {
					return ref // the content: the provider sends the reference, the host fills it in
				}
				info, ok := ready[m[1]]
				if !ok {
					missing = m[1]
					return ref
				}
				if m[2] == "name" {
					return info.Name
				}
				return info.SHA256
			})
			if missing != "" {
				return nil, fmt.Errorf("parameter %s names artifact %s, which is not ready (ask host/artifact first)", k, missing)
			}
			out[k] = append(out[k], filled)
		}
	}
	return out, nil
}

// ContentRefs are the artifacts whose content a value names ("{artifact:<id>.base64}").
func ContentRefs(v string) []string {
	var out []string
	for _, m := range contentRef.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1])
	}
	return out
}

// FillContent replaces the content references in a value with the base64 of each artifact's
// content (content returns it).
func FillContent(v string, content func(id string) (string, error)) (string, error) {
	var ferr error
	out := contentRef.ReplaceAllStringFunc(v, func(ref string) string {
		c, err := content(contentRef.FindStringSubmatch(ref)[1])
		if err != nil && ferr == nil {
			ferr = err
		}
		return c
	})
	return out, ferr
}

// Describe is an artifact as a plan shows it.
func (a Artifact) Describe() string {
	switch a.Kind {
	case ArtifactDownload:
		return fmt.Sprintf("%s from %s (sha256 %s), stored as %s", a.ID, a.URL, a.SHA256, a.Name)
	case ArtifactSeed:
		net := a.Network.Mode
		if a.Network.Mode == "static" {
			net = fmt.Sprintf("static %s via %s dns %s", a.Network.Address, a.Network.Gateway, strings.Join(a.Network.DNS, ","))
		}
		id, format := fmt.Sprintf("ID %d", a.NodeID), "a NoCloud ISO"
		if a.NodeID == 0 {
			id = "a number Mamela assigns"
		}
		if a.Format == SeedUserData {
			format = "user-data that fetches mamelet from the Control Plane"
		}
		return fmt.Sprintf("%s: the mamelet seed of node %s (%s, network %s) as %s, made by Mamela with a one-time enrollment token", a.ID, a.Node, id, net, format)
	}
	return a.ID
}

// Vouch is what Mamela makes of a provider's claim to own an object: claimed (Owned or
// Unfinished), it is Mamela's only if its IDs match those of an object Mamela recorded it set out
// to create for the resource (ADR 0004). A marker alone is not enough: anyone can copy one.
func Vouch(o Observed, created []map[string]string) Observed {
	claimed, unfinished := o.Owned || o.Unfinished, o.Unfinished
	o.Owned, o.Unfinished = false, false
	if !claimed || !o.Exists || len(o.IDs) == 0 {
		return o
	}
	for _, ids := range created {
		if sameObject(o.IDs, ids) {
			o.Owned, o.Unfinished = true, unfinished
			return o
		}
	}
	return o
}

// sameObject: the IDs of one are among those of the other, with the same values. A create is
// recorded with the IDs known before it (a Proxmox VMID, an EC2 client token); the object is
// later observed with those and maybe more (its instance ID), or with fewer (no storage).
func sameObject(observed, recorded map[string]string) bool {
	shared := 0
	for k, v := range observed {
		if w, ok := recorded[k]; ok {
			if w != v {
				return false
			}
			shared++
		}
	}
	return shared > 0 && (shared == len(observed) || shared == len(recorded))
}
