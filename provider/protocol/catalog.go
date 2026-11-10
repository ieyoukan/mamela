package protocol

// Effect is what an API call does, as the host knows it from its catalog.
type Effect string

const (
	EffectRead     Effect = "read"      // changes nothing
	EffectCreate   Effect = "create"    // creates an object
	EffectChange   Effect = "change"    // changes an object in place, losing no data
	EffectRestart  Effect = "restart"   // interrupts a running object
	EffectLoseData Effect = "lose-data" // can destroy data (delete, shrink, overwrite)
)

// Catalog is what the host knows about an API: the effect of each call. It belongs to the host
// (Mamela ships the catalogs of the APIs it knows), never to a provider: a provider's own word on
// what its calls do is not used for safety. A call that is not in the catalog is unclassified:
// an operation that sends one always needs a person's approval.
type Catalog struct {
	API     string  `json:"api"`
	Entries []Entry `json:"entries"`
}

// Entry is one call of an API and its effect.
type Entry struct {
	Method string `json:"method"`
	Path   string `json:"path"` // a template, as in Call
	Effect Effect `json:"effect"`
	// Precondition names the parameter that makes the API refuse the request when the object's
	// version differs (Proxmox's "digest"). The host requires it to equal the permit's
	// Precondition, when there is one.
	Precondition string `json:"precondition,omitempty"`
}

// Lookup returns the entry of a call (method and template, as declared).
func (c Catalog) Lookup(method, path string) (Entry, bool) {
	for _, e := range c.Entries {
		if e.Method == method && e.Path == path {
			return e, true
		}
	}
	return Entry{}, false
}

// Verified is what the host can tell about an operation's effects from its catalog.
type Verified struct {
	Effects      []Effect `json:"effects"`                // of the classified calls, without duplicates, in the order of Effects
	Unclassified []Call   `json:"unclassified,omitempty"` // calls the catalog does not know
}

// Verify classifies an operation's calls.
func (c Catalog) Verify(o Operation) Verified {
	var v Verified
	seen := map[Effect]bool{}
	for _, call := range o.Calls {
		e, ok := c.Lookup(call.Method, call.Path)
		if !ok || c.API == "" {
			v.Unclassified = append(v.Unclassified, call)
			continue
		}
		seen[e.Effect] = true
	}
	for _, e := range []Effect{EffectRead, EffectCreate, EffectChange, EffectRestart, EffectLoseData} {
		if seen[e] {
			v.Effects = append(v.Effects, e)
		}
	}
	return v
}

// MayLoseData reports whether a classified call may destroy data. Policy denies such an
// operation by default, whatever the provider says it does.
func (v Verified) MayLoseData() bool {
	return containsEffect(v.Effects, EffectLoseData)
}

// AutoApprovable reports whether policy may approve the operation without a person: every call is
// classified and every effect is one of auto. Anything unclassified needs a person.
func (v Verified) AutoApprovable(auto []Effect) bool {
	if len(v.Unclassified) > 0 {
		return false
	}
	for _, e := range v.Effects {
		if !containsEffect(auto, e) {
			return false
		}
	}
	return true
}

func containsEffect(es []Effect, e Effect) bool {
	for _, x := range es {
		if x == e {
			return true
		}
	}
	return false
}
