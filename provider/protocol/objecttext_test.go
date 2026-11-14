package protocol

import "testing"

func TestObjectText(t *testing.T) {
	for _, c := range []struct{ where, value, want string }{
		{"json", `"app:1"`, "app:1"},
		{"json", `["app:1"]`, "app:1"},
		{"json", `["app:1","app:2"]`, `["app:1","app:2"]`},
		{"json", `[]`, `[]`},
		{"json", `5`, `5`},
		{"form", `["app:1"]`, `["app:1"]`},
	} {
		if got := ObjectText(c.where, c.value); got != c.want {
			t.Errorf("ObjectText(%s, %s) = %s, want %s", c.where, c.value, got, c.want)
		}
	}
}
