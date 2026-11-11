package protocol

import (
	"encoding/json/jsontext"
	"fmt"
)

// Version is the version of the provider protocol: JSON-RPC 2.0, one message per line, on the
// provider program's standard input (from the host) and standard output (to the host). The
// provider writes nothing else to standard output; diagnostics go to standard error.
//
// The host sends one request at a time and waits for its response. While it waits, the provider
// may send host/request requests (one at a time) and must wait for each response.
const Version = "mamela-provider-rpc/v1"

// Methods the host calls on the provider.
const (
	// MethodDescribe returns the manifest. No host/request is allowed.
	MethodDescribe = "describe"
	// MethodObserve reads the world: only the manifest's ObserveCalls, and only reads.
	MethodObserve = "observe"
	// MethodDiff computes the changes from its parameters alone. No host/request is allowed, and
	// the same parameters must give the same result.
	MethodDiff = "diff"
	// MethodApply carries out one approved change under its permit.
	MethodApply = "apply"
	// MethodLookup says whether a change took effect, after an apply whose outcome is unknown.
	// Reads only, like observe, plus the reads of the change's operation.
	MethodLookup = "lookup"
)

// MethodHostRequest is the method the provider calls on the host to send an API request.
const MethodHostRequest = "host/request"

// MethodHostArtifact is the method the provider calls on the host, during apply, to have one of
// the change's artifacts made ready and to learn its name, sha256 and size (never its content).
const MethodHostArtifact = "host/artifact"

// Message is one JSON-RPC 2.0 message: a request (Method is set) or a response (Result or
// Error). Every request has an ID; there are no notifications.
type Message struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method,omitempty"`
	Params  jsontext.Value `json:"params,omitempty"`
	Result  jsontext.Value `json:"result,omitempty"`
	Error   *Error         `json:"error,omitempty"`
}

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// Error codes. The JSON-RPC ones, then the provider's and the host's.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	// CodeFailed: the provider could not do what was asked (its message says why).
	CodeFailed = -32000

	// Answers to host/request. Refused and NotSent guarantee that this one request was not
	// sent; they say nothing about earlier requests. Uncertain means it may have taken effect.
	CodeRefused   = -32010 // outside what the host allows now (the permit, observe, diff)
	CodeNotSent   = -32011 // the host could not send it (connection refused, ...)
	CodeUncertain = -32012 // sent, but the outcome is unknown (timeout, connection lost)
)

// DescribeParams is the host's greeting.
type DescribeParams struct {
	Protocol string `json:"protocol"` // Version
}

// DescribeResult is the provider's manifest and the protocol it speaks.
type DescribeResult struct {
	Protocol string   `json:"protocol"`
	Manifest Manifest `json:"manifest"`
}

// Resource is one declared resource of the provider's kinds.
type Resource struct {
	ID   string         `json:"id"` // the graph ID
	Kind ResourceKind   `json:"kind"`
	Name string         `json:"name"`
	Spec jsontext.Value `json:"spec,omitempty"` // the declared fields, in the kind's format
}

// ObserveParams lists the declared resources. The provider also reports, from what it reads,
// resources it created that are no longer declared (Owned), so that Mamela keeps them.
type ObserveParams struct {
	Resources []Resource `json:"resources"`
}

// Observed is the real state of one resource.
type Observed struct {
	ID     string       `json:"id"` // the graph ID
	Kind   ResourceKind `json:"kind"`
	Name   string       `json:"name"`
	Exists bool         `json:"exists"`
	// Owned is true when every mark of ownership the provider keeps is there (Proxmox: the marker,
	// the tag, the pool and the VMID range). Unfinished is true when only some are (a create that
	// was interrupted before its last step). Either is a claim: Mamela treats the object as its own
	// only if it matches what Mamela recorded it set out to create (ADR 0004).
	Owned      bool `json:"owned"`
	Unfinished bool `json:"unfinished,omitempty"`
	// IDs are the object's IDs in the API ({"item": "web"}).
	IDs map[string]string `json:"ids,omitempty"`
	// Version is the observed version (an ETag, Proxmox's config digest), the precondition of a
	// change based on this observation.
	Version string         `json:"version,omitempty"`
	State   jsontext.Value `json:"state,omitempty"`
	// Node is set for an existing resource of a node kind (Kind.Node): a machine mamelet runs on.
	Node *ObservedNode `json:"node,omitempty"`
}

// ObservedNode is what Mamela needs to know of a machine mamelet runs on (ADR 0004).
type ObservedNode struct {
	// ID identifies the machine in its revision and enrollment (Proxmox: the VMID).
	ID            int64 `json:"id"`
	Running       bool  `json:"running"`
	UptimeSeconds int64 `json:"uptimeSeconds,omitempty"`
}

// ObserveResult is the observed state.
type ObserveResult struct {
	Resources []Observed `json:"resources"`
}

// DiffParams are the declaration and the observation.
type DiffParams struct {
	Desired  []Resource `json:"desired"`
	Observed []Observed `json:"observed"`
}

// DiffResult lists the changes that make the world match the declaration, in order.
type DiffResult struct {
	Changes []Change `json:"changes"`
}

// Change is one planned operation on one resource. A person or the policy approves exactly this:
// the operation, the objects (IDs) and every changing request it sends (Requests). The host
// derives the permit from it and sends nothing else.
type Change struct {
	Resource string       `json:"resource"` // the graph ID
	Kind     ResourceKind `json:"kind"`
	Op       Op           `json:"op"`
	// IDs bind the operation to its objects: every changing request must name exactly these in
	// its path and in same-named parameters.
	IDs map[string]string `json:"ids"`
	// Requests are the changing requests, each with exactly the parameters it sends and their
	// values: a request that omits, adds or changes a parameter is refused. Each is sent at most
	// once. Reads are not listed: the operation's declared reads are allowed.
	Requests []Approved `json:"requests"`
	// Precondition is the observed Version the change is based on.
	Precondition string   `json:"precondition,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
	// Artifacts are the files the change needs, which the host makes ready and sends; the
	// provider never sees their content (ADR 0004).
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Approved is one approved changing request: a declared call and exactly the parameters it sends.
// A value may name an artifact's facts, "{artifact:<id>.name}" or "{artifact:<id>.sha256}": the
// host fills them in when it makes the artifact ready.
type Approved struct {
	Method string              `json:"method"`
	Path   string              `json:"path"` // the call's template
	Params map[string][]string `json:"params,omitempty"`
	// Upload is the artifact this request sends as its file (the call's File parameter).
	Upload string `json:"upload,omitempty"`
}

// ArtifactKind is what an artifact is and who makes it.
type ArtifactKind string

const (
	// ArtifactDownload is a file the host downloads from URL (https) and checks against SHA256.
	ArtifactDownload ArtifactKind = "download"
	// ArtifactSeed is a node's mamelet seed (cloud-init NoCloud): the host makes it, with a fresh
	// one-time enrollment token, for Node with NodeID and Network. Its name and sha256 are known
	// only when it is made.
	ArtifactSeed ArtifactKind = "seed"
)

// Artifact is one file a change needs.
type Artifact struct {
	ID   string       `json:"id"` // referenced as {artifact:<id>...} and by Approved.Upload
	Kind ArtifactKind `json:"kind"`
	// Download.
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Name   string `json:"name,omitempty"` // the file name to store it under
	// Seed.
	Node    string   `json:"node,omitempty"`
	NodeID  int64    `json:"nodeId,omitempty"`
	Network *Network `json:"network,omitempty"`
}

// Network is a node's IPv4 configuration, given at creation (Mamela's graph Network).
type Network struct {
	Mode    string   `json:"mode"` // "dhcp" or "static"
	Address string   `json:"address,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

// HostArtifact asks for one of the change's artifacts.
type HostArtifact struct {
	ID string `json:"id"`
}

// ArtifactInfo is a ready artifact, as the provider may know it.
type ArtifactInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ApplyParams is one change to carry out, with its permit and what it was planned from.
type ApplyParams struct {
	Permit   Permit    `json:"permit"`
	Change   Change    `json:"change"`
	Desired  *Resource `json:"desired,omitempty"`
	Observed *Observed `json:"observed,omitempty"`
}

// ApplyResult is a successful apply. A failed apply is a JSON-RPC error; the host decides the
// outcome (done, failed, partly-done, unknown) from the requests it sent, not from the provider.
type ApplyResult struct {
	Message string `json:"message,omitempty"`
}

// LookupParams asks whether a change took effect.
type LookupParams struct {
	Change Change `json:"change"`
}

// LookupStatus is what a lookup found.
type LookupStatus string

const (
	LookupDone    LookupStatus = "done"        // the change is in effect
	LookupNotDone LookupStatus = "not-done"    // nothing of it is in effect
	LookupPartly  LookupStatus = "partly-done" // some of it is
)

// LookupResult is what a lookup found.
type LookupResult struct {
	Status LookupStatus `json:"status"`
}

// HostRequest is an API request the provider asks the host to send. The host adds the base URL
// and the credentials; the provider never sees either.
type HostRequest struct {
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Query  map[string][]string `json:"query,omitempty"`
	Form   map[string][]string `json:"form,omitempty"` // sent as application/x-www-form-urlencoded
	// Upload names an artifact of the change: the host sends the form as multipart/form-data, with
	// the artifact as the call's File parameter.
	Upload string `json:"upload,omitempty"`
}

// HostResponse is the API's response.
type HostResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
	// Reason is the status line's text when the API puts its error there (Proxmox does).
	Reason string `json:"reason,omitempty"`
}
