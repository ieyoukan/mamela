package protocol

import "testing"

// Calls of a query API share a method and a path and are told apart by fixed parameters; a
// declaration where one request could be two calls is refused.
func TestFixedParameters(t *testing.T) {
	run := Call{Method: "POST", Path: "/", Form: []string{"Action", "MaxCount", "ImageId"}, Fixed: map[string]string{"Action": "RunInstances", "MaxCount": "1"}}
	start := Call{Method: "POST", Path: "/", Form: []string{"Action", "InstanceId.1"}, Fixed: map[string]string{"Action": "StartInstances"}}
	calls := []Call{run, start}
	if err := checkCalls(calls); err != nil {
		t.Fatal(err)
	}
	form := func(kv ...string) map[string][]string {
		m := map[string][]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = append(m[kv[i]], kv[i+1])
		}
		return m
	}
	if c, _, ok := FindCall(calls, "POST", "/", form("Action", "StartInstances", "InstanceId.1", "i-1"), true); !ok || !c.Same(start) {
		t.Fatal("StartInstances")
	}
	for name, params := range map[string]map[string][]string{
		"an undeclared action":     form("Action", "TerminateInstances", "InstanceId.1", "i-1"),
		"no action":                form("InstanceId.1", "i-1"),
		"a fixed value changed":    form("Action", "RunInstances", "MaxCount", "50", "ImageId", "ami-1"),
		"a fixed value sent twice": form("Action", "RunInstances", "Action", "TerminateInstances"),
	} {
		if _, _, ok := FindCall(calls, "POST", "/", params, true); ok {
			t.Errorf("%s: found a call", name)
		}
	}
	for name, bad := range map[string][]Call{
		"one without fixed": {run, {Method: "POST", Path: "/", Form: []string{"Action"}}},
		"indistinguishable": {run, {Method: "POST", Path: "/", Form: []string{"Action", "MaxCount"}, Fixed: map[string]string{"MaxCount": "1"}}},
		"fixed not sent":    {{Method: "POST", Path: "/", Fixed: map[string]string{"Action": "X"}}},
	} {
		if err := checkCalls(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := Manifest{Schema: Schema, Provider: "p", Version: "1", API: "ec2", Kinds: []Kind{{Kind: "I", Operations: []Operation{{Op: "start", Support: Supported, Calls: calls}}}}}
	ok := Change{Resource: "I a", Kind: "I", Op: "start", Requests: []Approved{{Method: "POST", Path: "/", Params: form("Action", "StartInstances", "InstanceId.1", "i-1")}}}
	if _, err := NewPermit(m, ok, "op", "", "", "x"); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Requests = []Approved{{Method: "POST", Path: "/", Params: form("Action", "TerminateInstances", "InstanceId.1", "i-1")}}
	if _, err := NewPermit(m, bad, "op", "", "", "x"); err == nil {
		t.Fatal("approved an undeclared action")
	}
}
