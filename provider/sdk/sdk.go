// Package sdk is for writing a Mamela provider in Go. A provider is a program: build it for
// WASI (GOOS=wasip1 GOARCH=wasm) and the host runs it isolated, with no files, environment or
// network; everything it sends to the API goes through Host.Request, which the host checks
// against the current permit and sends with credentials the provider never sees.
//
//	func main() { sdk.Serve(myProvider{}) }
//
// Test it with the conformance suite in provider/providertest. See docs/provider-authoring.md.
package sdk

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Provider is what a provider implements.
type Provider interface {
	// Manifest declares the provider: kinds, operations, the calls each sends, the capabilities
	// each needs.
	Manifest() protocol.Manifest
	// Observe reads the declared resources, and those it created that are no longer declared,
	// using only reads from the manifest's ObserveCalls.
	Observe(ctx context.Context, h *Host, p protocol.ObserveParams) (protocol.ObserveResult, error)
	// Diff computes the changes from its parameters alone (it has no Host). The same parameters
	// must give the same changes.
	Diff(ctx context.Context, p protocol.DiffParams) (protocol.DiffResult, error)
	// Apply carries out one change within its permit. It must resume a change that an earlier
	// apply left partly done: skip what is already in effect.
	Apply(ctx context.Context, h *Host, p protocol.ApplyParams) (protocol.ApplyResult, error)
	// Lookup says whether a change is in effect, using reads only.
	Lookup(ctx context.Context, h *Host, p protocol.LookupParams) (protocol.LookupResult, error)
}

// Guarder is a provider with a safety check around applies (ADR 0006): reads only.
type Guarder interface {
	Guard(ctx context.Context, h *Host, p protocol.GuardParams) (protocol.GuardResult, error)
}

// Preparer is a provider with explicit operations (deleting, rebooting): it makes their change
// from its parameters alone (it has no Host), as Diff does.
type Preparer interface {
	Prepare(ctx context.Context, p protocol.PrepareParams) (protocol.PrepareResult, error)
}

// Scripter is a provider that writes setup and maintenance scripts (Manifest.Scripts) from its
// parameters alone (it has no Host).
type Scripter interface {
	Script(ctx context.Context, p protocol.ScriptParams) (protocol.ScriptResult, error)
}

// Importer is a provider that turns objects of another tool (Terraform) into declarations
// (protocol.MethodImport) from its parameters alone (it has no Host).
type Importer interface {
	Import(ctx context.Context, p protocol.ImportParams) (protocol.ImportResult, error)
}

// Serve speaks the protocol on standard input and output until the host closes standard input.
// It exits the program with status 1 if the conversation breaks.
func Serve(p Provider) {
	if err := ServeIO(p, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "provider:", err)
		os.Exit(1)
	}
}

// maxLine is the longest message accepted.
const maxLine = 16 << 20

// ServeIO speaks the protocol on in and out. It returns nil when in ends.
func ServeIO(p Provider, in io.Reader, out io.Writer) error {
	c := &conn{in: bufio.NewReaderSize(in, 64<<10), out: out}
	for {
		m, err := c.read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if m.Method == "" {
			return fmt.Errorf("a response (id %d) when no request was pending", m.ID)
		}
		result, rerr := dispatch(p, c, m)
		reply := protocol.Message{JSONRPC: "2.0", ID: m.ID}
		if rerr != nil {
			var pe *protocol.Error
			if !errors.As(rerr, &pe) {
				pe = &protocol.Error{Code: protocol.CodeFailed, Message: rerr.Error()}
			}
			reply.Error = pe
		} else {
			reply.Result = result
		}
		if err := c.write(reply); err != nil {
			return err
		}
	}
}

func dispatch(p Provider, c *conn, m protocol.Message) (jsontext.Value, error) {
	ctx := context.Background()
	h := &Host{c: c}
	defer func() { h.c = nil }() // a Host works only during the call it was given to
	switch m.Method {
	case protocol.MethodDescribe:
		var in protocol.DescribeParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		if in.Protocol != protocol.Version {
			return nil, &protocol.Error{Code: protocol.CodeInvalidParams,
				Message: fmt.Sprintf("the host speaks %q; this provider speaks %q", in.Protocol, protocol.Version)}
		}
		return encode(protocol.DescribeResult{Protocol: protocol.Version, Manifest: p.Manifest()})
	case protocol.MethodObserve:
		var in protocol.ObserveParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(p.Observe(ctx, h, in))
	case protocol.MethodDiff:
		var in protocol.DiffParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(p.Diff(ctx, in))
	case protocol.MethodApply:
		var in protocol.ApplyParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(p.Apply(ctx, h, in))
	case protocol.MethodLookup:
		var in protocol.LookupParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(p.Lookup(ctx, h, in))
	case protocol.MethodGuard:
		g, ok := p.(Guarder)
		if !ok {
			return nil, &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "this provider has no guard"}
		}
		var in protocol.GuardParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(g.Guard(ctx, h, in))
	case protocol.MethodPrepare:
		pr, ok := p.(Preparer)
		if !ok {
			return nil, &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "this provider has no explicit operations"}
		}
		var in protocol.PrepareParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(pr.Prepare(ctx, in))
	case protocol.MethodScript:
		sc, ok := p.(Scripter)
		if !ok {
			return nil, &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "this provider writes no scripts"}
		}
		var in protocol.ScriptParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(sc.Script(ctx, in))
	case protocol.MethodImport:
		im, ok := p.(Importer)
		if !ok {
			return nil, &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "this provider imports nothing"}
		}
		var in protocol.ImportParams
		if err := decode(m.Params, &in); err != nil {
			return nil, err
		}
		return call(im.Import(ctx, in))
	default:
		return nil, &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "unknown method " + m.Method}
	}
}

func call[T any](v T, err error) (jsontext.Value, error) {
	if err != nil {
		return nil, err
	}
	return encode(v)
}

func decode(params jsontext.Value, v any) error {
	if err := json.Unmarshal(params, v); err != nil {
		return &protocol.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
	}
	return nil
}

func encode(v any) (jsontext.Value, error) {
	b, err := json.Marshal(v, json.Deterministic(true))
	return jsontext.Value(b), err
}

// Host sends API requests through the Mamela host. It is valid only during the call it was given
// to.
type Host struct {
	c *conn
}

// Request asks the host to send an API request and returns the API's response, whatever its
// status. The error is a *protocol.Error when the host did not send it (Refused, NotSent) or
// does not know whether it took effect (Uncertain); see the helpers below. An upload (Upload set)
// sends that artifact of the change as the call's file; ask for it with Artifact first.
func (h *Host) Request(ctx context.Context, r protocol.HostRequest) (protocol.HostResponse, error) {
	var resp protocol.HostResponse
	return resp, h.roundTrip(protocol.MethodHostRequest, r, &resp)
}

// Artifact has the host make one of the change's artifacts ready (during apply) and returns its
// name, sha256 and size. The provider never gets its content: the host sends it in an upload.
func (h *Host) Artifact(ctx context.Context, id string) (protocol.ArtifactInfo, error) {
	var info protocol.ArtifactInfo
	return info, h.roundTrip(protocol.MethodHostArtifact, protocol.HostArtifact{ID: id}, &info)
}

func (h *Host) roundTrip(method string, in, out any) error {
	if h == nil || h.c == nil {
		return errors.New("sdk: the Host is used outside the call it was given to")
	}
	params, err := encode(in)
	if err != nil {
		return err
	}
	h.c.next++
	id := h.c.next
	if err := h.c.write(protocol.Message{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	m, err := h.c.read()
	if err != nil {
		return fmt.Errorf("sdk: waiting for the host: %w", err)
	}
	if m.Method != "" || m.ID != id {
		return fmt.Errorf("sdk: the host sent %q (id %d) instead of the response to request %d", m.Method, m.ID, id)
	}
	if m.Error != nil {
		return m.Error
	}
	if err := json.Unmarshal(m.Result, out); err != nil {
		return fmt.Errorf("sdk: the host's response: %w", err)
	}
	return nil
}

// Refused reports whether the host refused the request (outside the permit, a change while
// observing, a request while diffing). It was not sent.
func Refused(err error) bool { return code(err) == protocol.CodeRefused }

// NotSent reports whether the request was not sent, refused or not.
func NotSent(err error) bool {
	c := code(err)
	return c == protocol.CodeRefused || c == protocol.CodeNotSent
}

// Uncertain reports whether the request may have taken effect although it failed.
func Uncertain(err error) bool { return code(err) == protocol.CodeUncertain }

func code(err error) int {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return 0
}

type conn struct {
	in   *bufio.Reader
	out  io.Writer
	next int64
}

func (c *conn) read() (protocol.Message, error) {
	var line []byte
	for {
		chunk, isPrefix, err := c.in.ReadLine()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return protocol.Message{}, io.ErrUnexpectedEOF
			}
			return protocol.Message{}, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return protocol.Message{}, fmt.Errorf("a message longer than %d bytes", maxLine)
		}
		if !isPrefix {
			break
		}
	}
	var m protocol.Message
	if err := json.Unmarshal(line, &m); err != nil {
		return protocol.Message{}, fmt.Errorf("reading a message: %w", err)
	}
	if m.JSONRPC != "2.0" {
		return protocol.Message{}, fmt.Errorf("not a JSON-RPC 2.0 message")
	}
	return m, nil
}

func (c *conn) write(m protocol.Message) error {
	b, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = c.out.Write(append(b, '\n'))
	return err
}
