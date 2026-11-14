package kit_test

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

type port struct {
	From int    `json:"from"`
	Mode string `json:"mode" values:"tcp|udp"`
}

type spec struct {
	Image   string   `json:"image" doc:"the image"`
	Subnet  string   `json:"subnet" mamela:"ref=AwsSubnet"`
	Groups  []string `json:"groups,omitempty" mamela:"ref=AwsSecurityGroup"`
	Public  *bool    `json:"public"`
	Ports   []port   `json:"ports,omitempty"`
	Ignored string   `json:"-"`
	hidden  string
}

func TestSpecOf(t *testing.T) {
	got := kit.SpecOf(spec{})
	if err := protocol.CheckFields(got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]protocol.Field{}
	for _, f := range got {
		byName[f.Name] = f
	}
	if len(got) != 5 || byName["Ignored"].Name != "" || byName["hidden"].Name != "" {
		t.Fatalf("fields: %+v", got)
	}
	if f := byName["image"]; f.Type != protocol.FieldString || f.Optional || f.Doc != "the image" {
		t.Errorf("image: %+v", f)
	}
	if f := byName["subnet"]; f.Type != protocol.FieldRef || f.Ref != "AwsSubnet" {
		t.Errorf("subnet: %+v", f)
	}
	if f := byName["groups"]; f.Type != protocol.FieldList || !f.Optional || f.Elem.Type != protocol.FieldRef || f.Elem.Ref != "AwsSecurityGroup" {
		t.Errorf("groups: %+v", f)
	}
	if f := byName["public"]; f.Type != protocol.FieldBool || !f.Optional {
		t.Errorf("public: %+v", f)
	}
	f := byName["ports"]
	if f.Type != protocol.FieldList || f.Elem.Type != protocol.FieldObject || f.Elem.TypeName != "port" || len(f.Elem.Fields) != 2 ||
		strings.Join(f.Elem.Fields[1].Values, "|") != "tcp|udp" {
		t.Errorf("ports: %+v", f)
	}
}

// What a manifest cannot give as a spec is refused, with the field named.
func TestSpecRefused(t *testing.T) {
	for name, v := range map[string]any{
		"name": struct {
			Name string `json:"name"`
		}{},
		"map": struct {
			M map[string]string `json:"m"`
		}{},
		"nested": struct {
			L [][]string `json:"l"`
		}{},
		"values": struct {
			N int `json:"n" values:"1|2"`
		}{},
	} {
		if err := protocol.CheckFields(kit.SpecOf(v)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	twice := []protocol.Field{{Name: "a", Type: protocol.FieldString}, {Name: "a", Type: protocol.FieldInt}}
	if err := protocol.CheckFields(twice); err == nil {
		t.Error("a field twice: accepted")
	}
}
