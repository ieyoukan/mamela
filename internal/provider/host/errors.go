// Package host runs providers that are separate programs, and is the only way they reach
// anything.
//
// Trust boundary (docs/adr/0002-provider-contract.md, decision 1): a provider is untrusted code.
// A WASM provider runs in the pinned wasmtime with standard input, output and error and nothing
// else: no directories, no environment, no network, no HTTP. Its imports are checked before it
// runs. A native provider runs only when the user trusted that exact executable (digest), and is
// then outside any sandbox: it can read whatever the user can.
//
// What the host enforces, whatever the provider does (decision 2):
//   - describe and diff send no request at all;
//   - observe and lookup send only declared reads that the host's catalog classifies as reads;
//   - apply sends only the calls of the approved operation, with declared parameters only;
//     changing requests (anything the catalog does not classify as a read) name exactly the
//     permit's IDs in their path and same-named parameters, pinned parameters have approved
//     values, the precondition parameter (when the catalog names one) carries the observed
//     version, and only before the permit expires; a permit is used once.
//
// What it does not enforce: values that are not pinned, request bodies other than the form,
// what an API does with an allowed request beyond its catalog entry, the targets of reads during
// apply, and preconditions of calls whose catalog entry names none.
//
// The host records every request and decides the outcome of an apply from the record (done,
// failed, partly-done, unknown), not from what the provider says.
package host

import (
	"fmt"
	"strings"
)

// Code is a stable reason code.
type Code string

const (
	CodeExecutable Code = "MAM-PRV-0001" // not a provider the host can run, or imports it does not allow
	CodeNotTrusted Code = "MAM-PRV-0002" // a native provider the user has not trusted
	CodeProtocol   Code = "MAM-PRV-0003" // the provider broke the protocol or its manifest is invalid
	CodeRefused    Code = "MAM-PRV-0004" // the provider asked for a request outside what is allowed
	CodePermit     Code = "MAM-PRV-0005" // the permit does not match the change, expired or was used
	CodeProvider   Code = "MAM-PRV-0006" // the provider program failed (exited, timed out)
	CodeFailed     Code = "MAM-PRV-0007" // the provider reported that it could not do what was asked
)

// Error is a failure written for a person.
type Error struct {
	Code    Code
	Message string
	Hint    string
	Detail  string // the provider's standard error, trimmed
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Code, e.Message)
	if e.Hint != "" {
		fmt.Fprintf(&b, "\n  hint: %s", e.Hint)
	}
	if d := strings.TrimSpace(e.Detail); d != "" {
		b.WriteString("\n  detail:\n")
		for _, line := range strings.Split(d, "\n") {
			b.WriteString("    " + line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
