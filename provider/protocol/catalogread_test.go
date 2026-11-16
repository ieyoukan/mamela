package protocol

import (
	"strings"
	"testing"
)

// A call that a catalog calls a read must be one by its HTTP method too: the host lets reads through
// without anyone's approval, so a catalog that calls a DELETE a read would let it through.
func TestCatalogReadsAreReadMethods(t *testing.T) {
	cat := func(entry string) (Catalog, error) {
		return DecodeCatalog([]byte(`{"schema":"mamela-catalog/v1","api":"x","entries":[{"method":"GET","path":"/a","effect":"read"},` + entry + `]}`))
	}
	for _, c := range []struct {
		name, entry, want string
	}{
		{"delete", `{"method":"DELETE","path":"/items/{item}","effect":"read"}`, "DELETE changes what it names"},
		{"put", `{"method":"PUT","path":"/items/{item}","effect":"read","form":["value"]}`, "PUT changes what it names"},
		{"patch", `{"method":"PATCH","path":"/items/{item}","effect":"read"}`, "PATCH changes what it names"},
		{"post without a fixed parameter", `{"method":"POST","path":"/items/{item}/purge","effect":"read"}`, "tells its calls apart by a fixed parameter"},
	} {
		_, err := cat(c.entry)
		if err == nil || !strings.Contains(err.Error(), "MAM-PRV-0011") || !strings.Contains(err.Error(), c.want) ||
			!strings.Contains(err.Error(), "entry 2 (") {
			t.Errorf("%s: %v (want MAM-PRV-0011, the entry's number and %q)", c.name, err, c.want)
		}
	}
	for name, entry := range map[string]string{
		"get":                   `{"method":"GET","path":"/b","effect":"read"}`,
		"an RPC-style post":     `{"method":"POST","path":"/","effect":"read","form":["Action"],"fixed":{"Action":"DescribeInstances"}}`,
		"a JSON-API post":       `{"method":"POST","path":"/","effect":"read","header":["X-Amz-Target"],"fixed":{"X-Amz-Target":"X.ListClusters"}}`,
		"a delete that says so": `{"method":"DELETE","path":"/items/{item}","effect":"lose-data"}`,
		"a put that changes":    `{"method":"PUT","path":"/items/{item}","effect":"change","form":["value"]}`,
	} {
		if _, err := cat(entry); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A catalog's error says which catalog and which entry, counted from 1 in the file's order: a
// catalog of hundreds of calls is read by a person who has to find the line.
func TestCatalogErrorsSayWhichEntry(t *testing.T) {
	good := `{"method":"GET","path":"/a","effect":"read"}`
	for name, c := range map[string]struct{ bad, want string }{
		"an unknown effect":   {`{"method":"GET","path":"/b","effect":"peek"}`, `entry 2 (GET /b): effect "peek"`},
		"a repeated call":     {`{"method":"GET","path":"/a","effect":"read"}`, "entry 2: GET /a twice"},
		"a bad path":          {`{"method":"GET","path":"b","effect":"read"}`, "entry 2: "},
		"a bind on a read":    {`{"method":"GET","path":"/c/{id}","effect":"read","bind":["id"]}`, "entry 2 (GET /c/{id}): bind id"},
		"a precondition":      {`{"method":"PUT","path":"/b","effect":"change","precondition":"v"}`, "entry 2 (PUT /b): the precondition v"},
		"values of nothing":   {`{"method":"PUT","path":"/b","effect":"change","values":{"x":["1"]}}`, "entry 2 (PUT /b): values for x"},
		"a bad measure":       {`{"method":"PUT","path":"/b","effect":"change","form":["n"],"measures":[{"param":"n","name":"cpu","format":"nope"}]}`, "entry 2 (PUT /b): measure"},
		"a read that deletes": {`{"method":"DELETE","path":"/b","effect":"read"}`, "MAM-PRV-0011: catalog x, entry 2 (DELETE /b)"},
	} {
		_, err := DecodeCatalog([]byte(`{"schema":"mamela-catalog/v1","api":"x","entries":[` + good + `,` + c.bad + `]}`))
		if err == nil || !strings.Contains(err.Error(), "catalog x") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want catalog x and %q)", name, err, c.want)
		}
	}
}
