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
	// Calls are the operation's calls from the manifest.
	Calls []Call `json:"calls"`
	// IDs every changing request must use (Change.IDs).
	IDs map[string]string `json:"ids"`
	// Values fix parameters (Change.Values).
	Values []Pin `json:"values,omitempty"`
	// Precondition is enforced on the calls whose catalog entry names a precondition parameter.
	Precondition string `json:"precondition,omitempty"`
	ExpiresAt    string `json:"expiresAt"` // RFC 3339, UTC
}

// NewPermit derives the permit of an approved change from the manifest. It fails when the change
// is not a supported operation of the manifest or pins a parameter its calls do not send.
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
	for _, p := range c.Values {
		i := slices.IndexFunc(o.Calls, func(call Call) bool { return call.Method == p.Method && call.Path == p.Path })
		if i < 0 || !slices.Contains(o.Calls[i].Params, p.Param) || len(p.Values) == 0 {
			return Permit{}, fmt.Errorf("value of %s on %s %s: the operation's calls do not send it, or no value", p.Param, p.Method, p.Path)
		}
	}
	return Permit{Schema: PermitSchema, OperationID: operationID, PlanDigest: planDigest, ProvidersDigest: providersDigest,
		Provider: m.Provider, Resource: c.Resource, Op: c.Op, Calls: slices.Clone(o.Calls), IDs: c.IDs,
		Values: slices.Clone(c.Values), Precondition: c.Precondition, ExpiresAt: expiresAt}, nil
}
