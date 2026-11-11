package protocol

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

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
	// Query and Form are where the API reads each parameter. The host sends a parameter only
	// where the API reads it, so that what it checks is what the API acts on.
	Query []string `json:"query,omitempty"`
	Form  []string `json:"form,omitempty"`
	// Precondition names the parameter that makes the API refuse the request when the object's
	// version differs (Proxmox's "digest"); it is one of Query or Form. The host requires it, in
	// that place, to equal the permit's Precondition, when there is one.
	Precondition string `json:"precondition,omitempty"`
	// File is the multipart parameter in which the API reads an uploaded file.
	File string `json:"file,omitempty"`
	// Fixed tells calls on the same method and path apart, as in Call.
	Fixed map[string]string `json:"fixed,omitempty"`
	// Header and JSON are where the API reads the other parameters, as in Call (ADR 0007).
	Header []string `json:"header,omitempty"`
	JSON   []string `json:"json,omitempty"`
	// Measures are the parameters that size what the call makes or changes (vCPUs, memory, disk):
	// the host checks their values against the site's limits, whatever the provider declares.
	Measures []Measure `json:"measures,omitempty"`
	// Values allow, for a parameter, only these values (as its text: a JSON string member without
	// its quotes): IAM's AttachRolePolicy may attach only the policies Mamela knows (ADR 0007).
	// The host refuses any other value, at plan and before sending.
	Values map[string][]string `json:"values,omitempty"`
	// Bind are the parameters that name the object a change acts on (EC2's "InstanceId.1"), when
	// the path does not. In a change, each must be one of the permit's IDs, by the same name and
	// with the same value: a provider cannot point an approved request at another object.
	Bind []string `json:"bind,omitempty"`
}

// Measure is one parameter that sizes something.
type Measure struct {
	Param string `json:"param"`
	// Name is what it measures: MeasureCPU, MeasureMemoryMiB or MeasureDiskGiB.
	Name string `json:"name"`
	// Format is how its value reads: FormatCount, FormatSize, FormatDrive or FormatTable.
	Format string `json:"format"`
	// Table is what each value measures, for FormatTable (an EC2 instance type's vCPUs). A value
	// not in it is refused.
	Table map[string]int64 `json:"table,omitempty"`
}

// What a measure measures.
const (
	MeasureCPU       = "cpu"
	MeasureMemoryMiB = "memoryMiB"
	MeasureDiskGiB   = "diskGiB"
)

// How a measure's value reads.
const (
	FormatCount = "count" // a whole number: "2", "2048"
	FormatSize  = "size"  // an absolute size: "10G", "512M", "1T" (in GiB, rounded up); "+5G" is refused
	FormatDrive = "drive" // a new disk "<storage>:<GiB>[,options]" (Proxmox); an import is 0 here, its growth is a size
	FormatTable = "table" // a name the measure's table gives a number ("t3.small" -> 2 vCPUs)
	// FormatCPUUnits: ECS CPU units, 1024 to a vCPU ("256" is a quarter: 1 vCPU, rounded up).
	FormatCPUUnits = "cpu-units"
)

// In says where the API reads a parameter of the call: "query", "form", or "".
func (e Entry) In(param string) string {
	return in(param, e.Query, e.Form, e.Header, e.JSON)
}

// Check verifies the catalog's own consistency.
func (c Catalog) Check() error {
	seen := map[string]bool{}
	byPath := map[string][]Call{}
	for _, e := range c.Entries {
		if err := checkCall(e.Method, e.Path, e.Query, e.Form, e.Header, e.JSON, e.Fixed); err != nil {
			return fmt.Errorf("catalog %s: %w", c.API, err)
		}
		if err := checkFixed(e.Method, e.Path, e.Fixed, e.In); err != nil {
			return fmt.Errorf("catalog %s: %w", c.API, err)
		}
		key := e.Method + " " + e.Path + " " + fixedKey(e.Fixed)
		if seen[key] {
			return fmt.Errorf("catalog %s: %s twice", c.API, key)
		}
		seen[key] = true
		byPath[e.Method+" "+e.Path] = append(byPath[e.Method+" "+e.Path], Call{Method: e.Method, Path: e.Path, Fixed: e.Fixed})
		if !containsEffect([]Effect{EffectRead, EffectCreate, EffectChange, EffectRestart, EffectLoseData}, e.Effect) {
			return fmt.Errorf("catalog %s: %s %s: effect %q", c.API, e.Method, e.Path, e.Effect)
		}
		for _, m := range e.Measures {
			if e.In(m.Param) == "" || !slices.Contains([]string{MeasureCPU, MeasureMemoryMiB, MeasureDiskGiB}, m.Name) ||
				!slices.Contains([]string{FormatCount, FormatSize, FormatDrive, FormatTable, FormatCPUUnits}, m.Format) || (m.Format == FormatTable) != (len(m.Table) > 0) {
				return fmt.Errorf("catalog %s: %s %s: measure %+v", c.API, e.Method, e.Path, m)
			}
		}
		for p, vs := range e.Values {
			if e.In(p) == "" || len(vs) == 0 {
				return fmt.Errorf("catalog %s: %s %s: values for %s, which is not one of its parameters", c.API, e.Method, e.Path, p)
			}
		}
		for _, b := range e.Bind {
			if e.In(b) == "" || e.Effect == EffectRead {
				return fmt.Errorf("catalog %s: %s %s: bind %s must be a parameter of a call that changes something", c.API, e.Method, e.Path, b)
			}
		}
		if e.Precondition != "" && e.In(e.Precondition) == "" {
			return fmt.Errorf("catalog %s: %s %s: the precondition %s is not one of its parameters", c.API, e.Method, e.Path, e.Precondition)
		}
	}
	if err := checkDistinct(byPath); err != nil {
		return fmt.Errorf("catalog %s: %w", c.API, err)
	}
	return nil
}

// Read reads a value of the measure.
func (m Measure) Read(value string) (int64, error) {
	if m.Format == FormatTable {
		n, ok := m.Table[value]
		if !ok {
			return 0, fmt.Errorf("%q is not one Mamela knows the %s of", value, m.Name)
		}
		return n, nil
	}
	return ReadMeasure(m.Format, value)
}

// ReadMeasure reads a measure's value.
func ReadMeasure(format, value string) (int64, error) {
	switch format {
	case FormatCPUUnits:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a number of CPU units", value)
		}
		return (n + 1023) / 1024, nil
	case FormatCount:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a whole number", value)
		}
		return n, nil
	case FormatSize:
		if value == "" || strings.ContainsAny(value[:1], "+-") {
			return 0, fmt.Errorf("size %q is not an absolute size", value)
		}
		mult := int64(1)
		switch value[len(value)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		n, err := strconv.ParseInt(strings.TrimRight(value, "KMGT"), 10, 64)
		if err != nil || n < 0 || n > 1<<40 {
			return 0, fmt.Errorf("size %q is not a size", value)
		}
		bytes := n * mult
		return (bytes + 1<<30 - 1) >> 30, nil
	case FormatDrive:
		head, _, _ := strings.Cut(value, ",")
		_, size, ok := strings.Cut(head, ":")
		n, err := strconv.ParseInt(size, 10, 64)
		if !ok || err != nil || n < 0 {
			return 0, fmt.Errorf("drive %q does not make a new disk of a stated size", value)
		}
		return n, nil
	}
	return 0, fmt.Errorf("unknown measure format %q", format)
}

// ErrValueNotAllowed: a parameter has a value its catalog entry does not list (Values): Mamela
// allows no other, whatever the change or the site's budget.
var ErrValueNotAllowed = errors.New("not one of the values Mamela allows")

// CheckLimits checks the measured parameters of a request to a call against limits (by measure
// name; absent or 0: no limit). A measured value that does not read is refused, limit or not.
func (e Entry) CheckLimits(params map[string][]string, limits map[string]int64) error {
	for _, p := range slices.Sorted(maps.Keys(e.Values)) {
		for _, v := range params[p] {
			if !slices.Contains(e.Values[p], ParamText(e.In(p), v)) {
				return fmt.Errorf("%s %s: %s=%s: %w (%s)", e.Method, e.Path, p, v, ErrValueNotAllowed, strings.Join(e.Values[p], ", "))
			}
		}
	}
	for _, m := range e.Measures {
		for _, v := range params[m.Param] {
			v = ParamText(e.In(m.Param), v)
			n, err := m.Read(v)
			if err != nil {
				return fmt.Errorf("%s %s: %s: %v", e.Method, e.Path, m.Param, err)
			}
			if max := limits[m.Name]; max > 0 && n > max {
				return fmt.Errorf("%s %s: %s=%s is %d %s, over the site's limit of %d", e.Method, e.Path, m.Param, v, n, m.Name, max)
			}
		}
	}
	return nil
}

// Lookup returns the entry of a call without fixed parameters (method and template).
func (c Catalog) Lookup(method, path string) (Entry, bool) {
	return c.LookupCall(Call{Method: method, Path: path})
}

// LookupCall returns the entry of a declared call: the same method, path and fixed parameters.
func (c Catalog) LookupCall(call Call) (Entry, bool) {
	call.API = "" // a catalog is one API's

	for _, e := range c.Entries {
		if (Call{Method: e.Method, Path: e.Path, Fixed: e.Fixed}).Same(call) {
			return e, true
		}
	}
	return Entry{}, false
}

// LookupApproved returns the entry of an approved request: its method and template, and the fixed
// parameters it carries.
func (c Catalog) LookupApproved(a Approved) (Entry, bool) {
	var calls []Call
	for _, e := range c.Entries {
		calls = append(calls, Call{Method: e.Method, Path: e.Path, Fixed: e.Fixed})
	}
	call, _, ok := FindCall(calls, "", a.Method, a.Path, a.Params, false)
	if !ok {
		return Entry{}, false
	}
	return c.LookupCall(call)
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
		e, ok := c.LookupCall(call)
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

// SortEffects puts effects in the order of Effects (read to lose-data).
func SortEffects(es []Effect) []Effect {
	var out []Effect
	for _, e := range []Effect{EffectRead, EffectCreate, EffectChange, EffectRestart, EffectLoseData} {
		if containsEffect(es, e) {
			out = append(out, e)
		}
	}
	return out
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
