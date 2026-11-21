package graph

import (
	"fmt"
	"strings"
)

// A declaration may leave a sha256 out, for the evaluator to take it from mamela.sum (docs/design/
// declaration-inputs.md, 2): the sha256 of what a node downloads, named by its URL. A kind's Decode
// says which member and which URL (SpecDecoder.FromSum); the Pome's files (Files.Sum, the same way
// in as contentFrom's texts) give the sha256, and DecodeWith fills it in before the graph is
// validated, so that the graph, its canonical form and its digest are those of a declaration that
// wrote the sha256 itself. A graph decoded otherwise (Decode: a stored one) has every sha256.

// Pin is a sha256 a resource leaves to mamela.sum.
type Pin struct {
	URL      string // what the node downloads: its line in mamela.sum
	Resource string // the resource's ID ("NodeArtifact web/artifact/usr/local/bin/app")
	Field    string // the member ("spec.sha256")
	// Key: what is downloaded is a signing key (an APT source's). Its sha256 says that it is the
	// file mamela pin downloaded, not that it is its vendor's key: a person compares the key's
	// fingerprint with the one its vendor publishes.
	Key bool
}

// MissingSums is the error of a declaration whose mamela.sum lacks a sha256 it leaves out.
type MissingSums struct {
	Pins []Pin // in the order of the resources in the document, each once
}

func (e *MissingSums) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mamela.sum has no sha256 for %d download%s the declaration leaves to it:", len(e.Pins), plural(len(e.Pins)))
	for _, p := range e.Pins {
		fmt.Fprintf(&b, "\n  %s (%s %s)", p.URL, p.Resource, p.Field)
	}
	return b.String()
}

// URLs are the URLs of the pins, each once, in the order of the pins.
func (e *MissingSums) URLs() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range e.Pins {
		if !seen[p.URL] {
			seen[p.URL] = true
			out = append(out, p.URL)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// standIn is the sha256 a missing one is validated with: the graph is checked in full before
// MissingSums is reported (a declaration Mamela refuses is not one to pin), and is never used.
const standIn = "0000000000000000000000000000000000000000000000000000000000000000"

// sumState is what one DecodeWith finds mamela.sum lacks.
type sumState struct {
	missing []Pin
	seen    map[Pin]bool
}

// Summed is whether the sha256 the spec leaves out may come from mamela.sum (DecodeWith, given the
// Pome's files): when it is not, the member is needed (MAM-GRAPH-0004), as it always was.
func (d *SpecDecoder) Summed() bool { return d.files != nil && d.sums != nil }

// FromSum takes the spec's member field, a sha256 the spec leaves out, from mamela.sum: the sha256
// of the bytes at url (what the node downloads), which set puts into the spec. key says the bytes
// are a signing key (Pin.Key). A graph not given the Pome's files (Decode) has no mamela.sum: the
// member is missing.
func (d *SpecDecoder) FromSum(field, url string, key bool, set func(sha256 string)) {
	if !d.Summed() {
		d.Need(false, field)
		return
	}
	if h, ok := d.files.Sum(url); ok {
		set(h)
		return
	}
	set(standIn)
	p := Pin{URL: url, Resource: d.id, Field: "spec." + field, Key: key}
	if !d.sums.seen[p] { // a resource declared twice the same way is one
		d.sums.seen[p] = true
		d.sums.missing = append(d.sums.missing, p)
	}
}
