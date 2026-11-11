package protocol

import (
	"errors"
	"fmt"
	"slices"
)

// PermitSchema names this version of the permit.
const PermitSchema = "mamela-permit/v1"

// Permit is what the host allows for one approved change, and nothing more. The host issues it
// for one apply, checks every host/request of that apply against its own copy, and voids it when
// the apply ends. The provider receives a copy so that it knows the limits; changing the copy
// changes nothing.
type Permit struct {
	Schema          string `json:"schema"`
	OperationID     string `json:"operationId"`
	PlanDigest      string `json:"planDigest"`
	ProvidersDigest string `json:"providersDigest"`
	Provider        string `json:"provider"`
	Resource        string `json:"resource"`
	Op              Op     `json:"op"`
	// Calls are the operation's calls from the manifest: its reads, and the calls Requests use.
	Calls []Call `json:"calls"`
	// IDs every changing request must use (Change.IDs).
	IDs map[string]string `json:"ids"`
	// Requests are the approved changing requests (Change.Requests): nothing else changes.
	Requests []Approved `json:"requests"`
	// Precondition is enforced on the calls whose catalog entry names a precondition parameter.
	Precondition string `json:"precondition,omitempty"`
	// Artifacts are the files the host makes ready for this change, and only these (ADR 0004).
	Artifacts []Artifact `json:"artifacts,omitempty"`
	ExpiresAt string     `json:"expiresAt"` // RFC 3339, UTC
}

// NewPermit derives the permit of an approved change from the manifest. It fails when the change
// is not a supported operation of the manifest, or approves a request its calls do not declare.
func NewPermit(m Manifest, c Change, operationID, planDigest, providersDigest, expiresAt string) (Permit, error) {
	o, ok := m.Operation(c.Kind, c.Op)
	if !ok || o.Support != Supported {
		return Permit{}, fmt.Errorf("%s %s is not a supported operation of %s", c.Kind, c.Op, m.Provider)
	}
	if operationID == "" || expiresAt == "" {
		return Permit{}, errors.New("a permit needs an operation ID and an expiry")
	}
	for k := range c.IDs {
		if !validName(k) {
			return Permit{}, fmt.Errorf("ID name %q", k)
		}
	}
	if err := CheckArtifacts(c); err != nil {
		return Permit{}, err
	}
	for _, a := range c.Requests {
		call, _, ok := FindCall(o.Calls, a.Method, a.Path, a.Params, false)
		if !ok {
			return Permit{}, fmt.Errorf("request %s %s is not a call of %s %s (or fits more than one)", a.Method, a.Path, c.Kind, c.Op)
		}
		if (a.Upload != "") != (call.File != "") {
			return Permit{}, fmt.Errorf("request %s %s: an upload call sends exactly one artifact, and only an upload call sends one", a.Method, a.Path)
		}
		if _, ok := a.Params[call.File]; ok && call.File != "" {
			return Permit{}, fmt.Errorf("request %s %s: the file parameter %s is the host's to fill", a.Method, a.Path, call.File)
		}
		for name, vals := range a.Params {
			if call.In(name) == "" || len(vals) == 0 {
				return Permit{}, fmt.Errorf("request %s %s: parameter %s is not declared for the call, or has no value", a.Method, a.Path, name)
			}
		}
	}
	return Permit{Schema: PermitSchema, OperationID: operationID, PlanDigest: planDigest, ProvidersDigest: providersDigest,
		Provider: m.Provider, Resource: c.Resource, Op: c.Op, Calls: slices.Clone(o.Calls), IDs: c.IDs,
		Requests: slices.Clone(c.Requests), Precondition: c.Precondition, Artifacts: slices.Clone(c.Artifacts), ExpiresAt: expiresAt}, nil
}
