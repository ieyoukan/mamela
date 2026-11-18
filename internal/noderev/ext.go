package noderev

import (
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/internal/names"
)

// The generic part of the wire (stage 5 of docs/design/node-extensions.md): a node extension that
// does not use members of its own (k3s does, from before) has a part of the revision
// (Revision.Ext[name]) and a part of the report (Report.Ext[name]), in a JSON of its own that the
// core signs and stores without looking into it.
//
// Capability negotiation: a mamelet lists the extensions it takes in its report (Report.Extensions,
// with the version of the part it reads), and the Control Plane puts an extension's part into a
// node's revision only when the node's latest report lists it. A mamelet older than the generic wire
// (one that decodes revisions rejecting unknown members) therefore never gets an ext member, and
// keeps working. A mamelet that gets a part it does not take refuses that part only
// (Report.ExtRefused) and goes on with the rest.
//
// The one-time grants in a part are the core's to hand out: their deadlines (Once.Expires) are the
// Control Plane's bookkeeping, taken out before a revision is signed (ForMamelet). The extension
// owns the format of its part, so it registers how to find its grants (Part.Grants) and how to take
// them out or clear their deadlines (Part.ForMamelet).

// Part is how the core reads the one-time grants in a node extension's part of a revision.
type Part struct {
	// Version is the version of the part this Control Plane writes: a mamelet takes the part when its
	// report lists the extension at this version or a later one (Takes).
	Version int
	// Grants are the one-time grants in the part, with their deadlines. An error: the part cannot be
	// read, and is not signed.
	Grants func(part jsontext.Value) ([]Grant, error)
	// ForMamelet is the part as the Control Plane hands it to mamelet at now: every grant whose
	// deadline has passed (or that has none) taken out, the deadline of every other one cleared
	// (HandOut does both for one grant). An error: the part is not handed out.
	ForMamelet func(part jsontext.Value, now time.Time) (jsontext.Value, error)
}

var parts = map[string]Part{}

// RegisterPart says how the core reads the grants of the extension's part of a revision. It panics
// when the name is not an extension's name or is taken, or a function is missing: programming
// errors, found when the program starts.
func RegisterPart(name string, p Part) {
	if !names.Extension(name) {
		panic(fmt.Sprintf("noderev: %q is not a name of an extension", name))
	}
	if _, taken := parts[name]; taken || p.Version < 1 || p.Grants == nil || p.ForMamelet == nil {
		panic(fmt.Sprintf("noderev: extension %s: a name of its own, a version, Grants and ForMamelet are needed", name))
	}
	parts[name] = p
}

// PartVersion is the version of the part of extension name this Control Plane writes (0: it has
// no part of that name).
func PartVersion(name string) int { return parts[name].Version }

// Takes reports whether the mamelet that sent r takes the part of extension name this Control Plane
// writes: its report lists the extension at the part's version or a later one. It is the one test of
// capability negotiation, for publishing a revision, granting into a part and handing a revision out
// (r nil: no report, so nothing is taken). The report must be the node's current one (sent under its
// current registration): an older one says what another mamelet took.
func Takes(r *Report, name string) bool {
	p, ok := parts[name]
	return ok && r != nil && r.Extensions[name] >= p.Version
}

// KeepTaken takes out of the revision every extension's part the mamelet that sent r does not take
// (Takes): such a mamelet would refuse the whole revision for a member it does not know, or the part.
// The map is a new one: a revision copied from another shares its map.
func (rev *Revision) KeepTaken(r *Report) {
	var ext map[string]jsontext.Value
	for name, part := range rev.Ext {
		if Takes(r, name) {
			if ext == nil {
				ext = map[string]jsontext.Value{}
			}
			ext[name] = part
		}
	}
	rev.Ext = ext
}

// HandOut is a one-time grant as mamelet gets it at now: nil when its deadline has passed or it has
// none (a grant an older Control Plane issued), otherwise a copy without the deadline.
func HandOut(o *Once, now time.Time) *Once {
	if o == nil || o.Expires.IsZero() || !now.Before(o.Expires) {
		return nil
	}
	c := *o
	c.Expires = time.Time{}
	return &c
}

// ValidOnce reports whether a one-time grant names an operation and an attempt, and is for action:
// what Check asks of the core's grants, for an extension's.
func ValidOnce(o Once, action string) bool {
	return operationPattern.MatchString(o.Operation) && o.Attempt >= 1 && o.Action == action
}

// MaxPart bounds one extension's part of a revision, in bytes (its JSON).
const MaxPart = 4 << 20

// checkExt checks the ext member of a revision: names of extensions, each part a JSON object of
// bounded size. What is in a part is its extension's to check.
func checkExt(ext map[string]jsontext.Value, add func(format string, args ...any)) {
	for _, name := range slices.Sorted(maps.Keys(ext)) {
		part := ext[name]
		switch {
		case !names.Extension(name):
			add("ext %q is not a name of an extension", name)
		case len(part) > MaxPart:
			add("ext %s: the part is %d bytes, more than %d", name, len(part), MaxPart)
		case !part.IsValid() || part.Kind() != '{':
			add("ext %s: the part is not a JSON object", name)
		}
	}
}

// extForMamelet is the ext member as mamelet gets it at now: the part of an extension this Control
// Plane does not know is not handed out (nothing could tell its grants), nor a part whose grants
// cannot be read.
func extForMamelet(ext map[string]jsontext.Value, now time.Time) map[string]jsontext.Value {
	if len(ext) == 0 {
		return ext
	}
	out := map[string]jsontext.Value{}
	for name, part := range ext {
		p, ok := parts[name]
		if !ok {
			continue
		}
		if c, err := p.ForMamelet(part, now); err == nil {
			out[name] = c
		}
	}
	return out
}

// extGrants are the one-time grants of the parts this Control Plane knows.
func extGrants(ext map[string]jsontext.Value) []Grant {
	var out []Grant
	for _, name := range slices.Sorted(maps.Keys(ext)) {
		if p, ok := parts[name]; ok {
			if gs, err := p.Grants(ext[name]); err == nil {
				out = append(out, gs...)
			}
		}
	}
	return out
}

// signableExt says why the ext member cannot be signed: a part of an extension the Control Plane
// does not know, or whose grants it cannot read (it could not tell that none keeps its deadline).
func signableExt(ext map[string]jsontext.Value) error {
	for _, name := range slices.Sorted(maps.Keys(ext)) {
		p, ok := parts[name]
		if !ok {
			return fmt.Errorf("the revision has a part of extension %s, which this Control Plane does not know", name)
		}
		if _, err := p.Grants(ext[name]); err != nil {
			return fmt.Errorf("the part of extension %s cannot be read: %w", name, err)
		}
	}
	return nil
}
