package tfimport

import (
	"strings"
	"testing"
)

const state = `{
  "format_version": "1.0",
  "terraform_version": "1.9.0",
  "values": {
    "root_module": {
      "resources": [
        {"address": "kv_item.web", "mode": "managed", "type": "kv_item", "name": "web", "provider_name": "registry.terraform.io/example/kv",
         "values": {"id": "web", "value": "v1", "labels": ["prod"]}},
        {"address": "data.kv_item.lookup", "mode": "data", "type": "kv_item", "name": "lookup", "values": {"id": "x"}},
        {"address": "kv_item.many[0]", "mode": "managed", "type": "kv_item", "name": "many", "index": 0, "values": {"id": "m0"}},
        {"address": "kv_item.keyed[\"a b\"]", "mode": "managed", "type": "kv_item", "name": "keyed", "index": "a b", "values": {"id": "ka"}}
      ],
      "child_modules": [
        {"address": "module.net", "resources": [
          {"address": "module.net.kv_item.web", "mode": "managed", "type": "kv_item", "name": "web", "values": {"id": "nw"}}
        ], "child_modules": [
          {"address": "module.net.module.inner[1]", "resources": [
            {"address": "module.net.module.inner[1].kv_item.web", "mode": "managed", "type": "kv_item", "name": "web", "values": {"id": "niw"}}
          ]}
        ]},
        {"address": "module.web", "resources": [
          {"address": "module.web.kv_item.x", "mode": "managed", "type": "kv_item", "name": "x", "values": {"id": "c1"}}
        ]}
      ]
    }
  }
}`

func TestRead(t *testing.T) {
	objs, err := Read([]byte(state))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.Address+"="+o.Name)
	}
	want := []string{
		`kv_item.keyed["a b"]=keyed-a-b`,
		`kv_item.many[0]=many-0`,
		`kv_item.web=web`,
		`module.net.kv_item.web=net-web`,
		`module.net.module.inner[1].kv_item.web=net-inner-1-web`,
		`module.web.kv_item.x=web-x`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if string(objs[2].Values) != `{"id": "web", "value": "v1", "labels": ["prod"]}` {
		t.Errorf("values: %s", objs[2].Values)
	}
}

// Names never collide within a type.
func TestUniqueNames(t *testing.T) {
	objs, err := Read([]byte(`{"format_version":"1.0","values":{"root_module":{"resources":[
	  {"address":"t.a_b","mode":"managed","type":"t","name":"a_b","values":{}},
	  {"address":"t.a_b[\"x\"]","mode":"managed","type":"t","name":"a_b","index":"x","values":{}},
	  {"address":"t.a_b-x","mode":"managed","type":"t","name":"a_b-x","values":{}}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range objs {
		if seen[o.Name] {
			t.Errorf("%s twice", o.Name)
		}
		seen[o.Name] = true
	}
}

func TestRefused(t *testing.T) {
	for name, in := range map[string]string{
		"not json":   `terraform {`,
		"state file": `{"version": 4, "resources": []}`,
		"plan":       `{"format_version":"1.2","planned_values":{}}`,
	} {
		if _, err := Read([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
