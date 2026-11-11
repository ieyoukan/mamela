// Command rogue is the example provider with one misbehavior, chosen at build time
// (-ldflags=-X main.mode=...), for the host's tests: the host must refuse or detect each.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvprovider"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

var mode string

// readFile is set by readfile.go (build tag readfile): a provider that reads files.
var readFile func() string

type rogue struct{ kvprovider.Provider }

func (r rogue) Manifest() protocol.Manifest {
	m := r.Provider.Manifest()
	if mode == "env" {
		home, _ := os.LookupEnv("HOME")
		m.Version = fmt.Sprintf("env-%d-%q", len(os.Environ()), home)
	}
	if readFile != nil {
		m.Version = readFile()
	}
	m.Scripts = []protocol.Script{{Name: "setup", Description: "prints the settings it was given"}}
	return m
}

// Script writes a script from its parameters; mode script-empty writes nothing.
func (r rogue) Script(_ context.Context, p protocol.ScriptParams) (protocol.ScriptResult, error) {
	if mode == "script-empty" {
		return protocol.ScriptResult{}, nil
	}
	return protocol.ScriptResult{Text: fmt.Sprintf("# %s for %s\necho %s\n", p.Name, p.Credentials.TokenID, p.Settings)}, nil
}

func (r rogue) Observe(ctx context.Context, h *sdk.Host, p protocol.ObserveParams) (protocol.ObserveResult, error) {
	switch mode {
	case "observe-change":
		h.Request(ctx, protocol.HostRequest{Method: "PUT", Path: "/items/sneaky", Form: map[string][]string{"value": {"x"}}})
	case "observe-undeclared":
		h.Request(ctx, protocol.HostRequest{Method: "GET", Path: "/items/web"})
	case "garbage":
		fmt.Println("this is not JSON-RPC")
	case "hang":
		time.Sleep(time.Hour)
	}
	return r.Provider.Observe(ctx, h, p)
}

func (r rogue) Diff(ctx context.Context, p protocol.DiffParams) (protocol.DiffResult, error) {
	out, err := r.Provider.Diff(ctx, p)
	switch mode {
	case "diff-undeclared":
		out.Changes = append(out.Changes, protocol.Change{Resource: "KVItem other", Kind: kvprovider.Kind, Op: kvprovider.OpCreate,
			IDs: map[string]string{"item": "other"}})
	case "diff-precondition":
		for i := range out.Changes {
			out.Changes[i].Precondition = "999"
		}
	case "diff-no-precondition":
		// Leaves out the precondition and the version: would overwrite whatever is there.
		for i := range out.Changes {
			out.Changes[i].Precondition = ""
			for _, a := range out.Changes[i].Requests {
				delete(a.Params, "version")
			}
		}
	}
	return out, err
}

func (r rogue) Apply(ctx context.Context, h *sdk.Host, p protocol.ApplyParams) (protocol.ApplyResult, error) {
	put := func(path string, form map[string][]string) error {
		resp, err := h.Request(ctx, protocol.HostRequest{Method: "PUT", Path: path, Form: form})
		if err == nil && resp.Status != 200 {
			err = fmt.Errorf("status %d", resp.Status)
		}
		return err
	}
	name := p.Change.IDs["item"]
	switch mode {
	case "other-object":
		return protocol.ApplyResult{}, put("/items/other", map[string][]string{"value": {"v1"}, "owner": {"mamela"}})
	case "extra-param":
		return protocol.ApplyResult{}, put("/items/"+name, map[string][]string{"value": {"v1"}, "owner": {"mamela"}, "admin": {"1"}})
	case "unapproved-value":
		return protocol.ApplyResult{}, put("/items/"+name, map[string][]string{"value": {"evil"}, "owner": {"mamela"}})
	case "ignore-refusal":
		put("/items/other", map[string][]string{"value": {"v1"}, "owner": {"mamela"}})
		return protocol.ApplyResult{Message: "all good"}, nil
	}
	return r.Provider.Apply(ctx, h, p)
}

func main() {
	if mode == "deaf" {
		// Answers describe, then never reads its input again.
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		sdk.ServeIO(rogue{}, strings.NewReader(line), os.Stdout)
		time.Sleep(time.Hour)
	}
	sdk.Serve(rogue{})
}
