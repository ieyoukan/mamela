package protocol

import (
	"encoding/json/jsontext"
	"fmt"
	"regexp"
)

// MethodImport turns objects another tool manages (resources of a Terraform or OpenTofu state)
// into declarations of the provider's kinds, from its parameters alone: no host/request, like
// diff (ADR 0009). It changes nothing: what it returns is written as Emela for a person to read,
// and each object becomes Mamela's only through the provider's adopt operation (mamela act).
const MethodImport = "import"

// ImportParams are the objects to import: those of the types the provider's kinds import
// (Kind.Imports), all at once, so that a reference between two of them (a subnet's vpc_id) can be
// turned into a reference between their declarations.
type ImportParams struct {
	Objects []ImportObject `json:"objects"`
}

// ImportObject is one object of another tool.
type ImportObject struct {
	// Address is where the tool keeps it ("module.net.aws_vpc.main[0]").
	Address string `json:"address"`
	// Type is the tool's type of it ("aws_vpc").
	Type string `json:"type"`
	// Name is the name Mamela proposes for its declaration, from the address ("net-main-0").
	Name string `json:"name"`
	// Values are its attributes, as the tool records them.
	Values jsontext.Value `json:"values"`
}

// ImportResult is what the provider makes of the objects.
type ImportResult struct {
	Resources []Imported   `json:"resources"`
	Skipped   []ImportSkip `json:"skipped,omitempty"`
}

// Imported is the declaration of one object, and the IDs that identify the object at the API (what
// observe looks for to report it as adoptable).
type Imported struct {
	Address string            `json:"address"`
	Kind    ResourceKind      `json:"kind"`
	Name    string            `json:"name"`
	Spec    jsontext.Value    `json:"spec"`
	IDs     map[string]string `json:"ids"`
}

// ImportSkip is an object the provider does not import, and why (what to do about it).
type ImportSkip struct {
	Address string `json:"address"`
	Why     string `json:"why"`
}

// Adopting is a declared resource that is to be adopted (an import): observe reports the object
// with these IDs as Adoptable for it, though it carries no mark of Mamela's.
type Adopting struct {
	Resource string            `json:"resource"` // the graph ID ("AwsVpc main")
	IDs      map[string]string `json:"ids"`
}

var importTypeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func checkImports(k Kind) error {
	seen := map[string]bool{}
	for _, t := range k.Imports {
		if !importTypeRe.MatchString(t) || seen[t] {
			return fmt.Errorf("%s imports %q: a type of lower case letters, digits and _, once", k.Kind, t)
		}
		seen[t] = true
	}
	return nil
}
