package linux

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

const vm = `{"kind":"TestVM","name":"web","provider":"test","spec":{"network":{"mode":"dhcp"}}}`

func fileRes(name, spec string) string {
	return `{"kind":"NodeFile","name":"` + name + `","spec":` + spec + `}`
}

func decodeGraph(resources ...string) (*graph.Graph, error) {
	return graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + strings.Join(append([]string{vm}, resources...), ",") + `]}`))
}

// The defaults fill what is left out, and the canonical form writes them: two declarations of the
// same file are the same graph.
func TestFileDefaultsAndCanonicalForm(t *testing.T) {
	short, err := decodeGraph(fileRes("web/file/etc/motd", `{"node":"web","path":"/etc/motd","content":"hi\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	long, err := decodeGraph(fileRes("web/file/etc/motd", `{"node":"web","path":"/etc/motd","content":"hi\n","mode":"0644","owner":"root","group":"root","restart":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if short.Digest() != long.Digest() {
		t.Errorf("the defaults make another graph:\n%s\n%s", short.Canonical(), long.Canonical())
	}
	want := `{"kind":"NodeFile","name":"web/file/etc/motd","spec":{"content":"hi\n","group":"root","mode":"0644","node":"web","owner":"root","path":"/etc/motd"}}`
	if !strings.Contains(string(short.Canonical()), want) {
		t.Errorf("canonical %s", short.Canonical())
	}
	f := FileOf(short.Resources[1])
	if f == nil || f.Mode != "0644" || f.Owner != "root" || f.Group != "root" || f.Restart != nil || short.Resources[1].Node() != "web" {
		t.Errorf("%+v", f)
	}
	// After the provider's kinds (machines): the linux kinds come last.
	if s := short.Sorted(); s[1].Kind != KindFile {
		t.Errorf("order %v", s)
	}
}

// What a declaration of a file may not be, with the code each problem has.
func TestFileRejections(t *testing.T) {
	big := strings.Repeat("a", MaxContent+1)
	for _, c := range []struct {
		name, res string
		code      graph.Code
		says      string
	}{
		{"no content", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x"}`), graph.CodeMissingField, "spec.content"},
		{"unknown member", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","user":"root"}`), graph.CodeUnknownField, "user"},
		{"null mode", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","mode":null}`), graph.CodeWrongType, "mode"},
		{"numeric mode", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","mode":644}`), graph.CodeWrongType, "mode"},
		{"null restart", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","restart":null}`), graph.CodeWrongType, "restart"},
		{"restart not a list", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","restart":"nginx"}`), graph.CodeWrongType, "restart"},
		{"relative path", fileRes("web/fileetc/x", `{"node":"web","path":"etc/x","content":""}`), graph.CodeInvalidValue, "not an absolute path"},
		{"dot dot", fileRes("web/file/etc/../x", `{"node":"web","path":"/etc/../x","content":""}`), graph.CodeInvalidValue, "not a clean path"},
		{"forbidden", fileRes("web/file/etc/sudoers.d/app", `{"node":"web","path":"/etc/sudoers.d/app","content":""}`), graph.CodeInvalidValue, "become root"},
		{"authorized_keys", fileRes("web/file/root/.ssh/authorized_keys", `{"node":"web","path":"/root/.ssh/authorized_keys","content":""}`), graph.CodeInvalidValue, "NodeUser"},
		{"name", fileRes("web/etc/x", `{"node":"web","path":"/etc/x","content":""}`), graph.CodeNameMismatch, `"web/file/etc/x"`},
		{"node", fileRes("db/file/etc/x", `{"node":"db","path":"/etc/x","content":""}`), graph.CodeDanglingNode, "db"},
		{"too large", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"`+big+`"}`), graph.CodeInvalidValue, "at most 262144"},
		{"NUL", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"a\u0000b"}`), graph.CodeInvalidValue, "not text"},
		{"escape", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"a\u001b[2Jb"}`), graph.CodeUnsafeText, "content"},
		{"bidi", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"a‮b"}`), graph.CodeUnsafeText, "content"},
		{"setuid", fileRes("web/file/usr/local/bin/x", `{"node":"web","path":"/usr/local/bin/x","content":"","mode":"4755"}`), graph.CodeInvalidValue, "setuid"},
		{"world-writable", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","mode":"0666"}`), graph.CodeInvalidValue, "every user"},
		{"owner", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","owner":"Root"}`), graph.CodeInvalidValue, "user's name"},
		{"group", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","group":"0"}`), graph.CodeInvalidValue, "group's name"},
		{"restart name", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","restart":["-x"]}`), graph.CodeInvalidValue, "service name"},
		{"restart twice", fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"","restart":["nginx","nginx.service"]}`), graph.CodeInvalidValue, "twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeGraph(c.res)
			var ge *graph.Error
			if !errors.As(err, &ge) {
				t.Fatalf("accepted (%v)", err)
			}
			for _, p := range ge.Problems {
				if p.Code == c.code && strings.Contains(p.Path+" "+p.Message+" "+p.Hint, c.says) {
					return
				}
			}
			t.Errorf("no %s saying %q in %v", c.code, c.says, err)
		})
	}
}

// One node, one path: a file is declared once, and a path is not both a file and a directory.
func TestFileConflicts(t *testing.T) {
	_, err := decodeGraph(
		fileRes("web/file/etc/app", `{"node":"web","path":"/etc/app","content":"a"}`),
		fileRes("web/file/etc/app/x.conf", `{"node":"web","path":"/etc/app/x.conf","content":"b"}`))
	if err == nil || !strings.Contains(err.Error(), string(graph.CodeConflict)) || !strings.Contains(err.Error(), "file or a directory") {
		t.Errorf("a file and a directory: %v", err)
	}
	_, err = decodeGraph(
		fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"a"}`),
		fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"b"}`))
	if err == nil || !strings.Contains(err.Error(), string(graph.CodeDuplicateResource)) {
		t.Errorf("one path twice: %v", err)
	}
	// The same path on two nodes is two files.
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm + `,` +
		`{"kind":"TestVM","name":"db","provider":"test","spec":{"network":{"mode":"dhcp"}}},` +
		fileRes("web/file/etc/x", `{"node":"web","path":"/etc/x","content":"a"}`) + `,` +
		fileRes("db/file/etc/x", `{"node":"db","path":"/etc/x","content":"a"}`) + `]}`))
	if err != nil || len(g.Resources) != 4 {
		t.Errorf("two nodes: %v", err)
	}
}

// The approved writes of a node's files ride in its revision: all of them together are bounded.
func TestFileContentPerNode(t *testing.T) {
	var res []string
	part := strings.Repeat("x", MaxContent)
	for i := range MaxNodeContent/MaxContent + 1 {
		res = append(res, fileRes(fmt.Sprintf("web/file/etc/f%d", i), fmt.Sprintf(`{"node":"web","path":"/etc/f%d","content":"%s"}`, i, part)))
	}
	_, err := decodeGraph(res...)
	if err == nil || !strings.Contains(err.Error(), "hold more than 1048576 bytes") {
		t.Errorf("%v", err)
	}
	if _, err := decodeGraph(res[:len(res)-1]...); err != nil {
		t.Errorf("exactly the bound: %v", err)
	}
}

// Warnings: content that looks like a secret, and a restart of a service the declaration does not
// have on the node.
func TestFileWarnings(t *testing.T) {
	key := `-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n`
	g, err := decodeGraph(
		fileRes("web/file/etc/app/key", `{"node":"web","path":"/etc/app/key","content":"`+key+`","mode":"0600"}`),
		fileRes("web/file/etc/app/token", `{"node":"web","path":"/etc/app/token","content":"token = ghp_`+strings.Repeat("A", 36)+`\n"}`),
		fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","content":"password = changeme\n","restart":["app","nginx"]}`),
		`{"kind":"NodeService","name":"web/nginx","spec":{"node":"web","service":"nginx","enabled":true,"running":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range g.Warnings() {
		got = append(got, string(w.Code)+" "+w.Resource+": "+w.Message)
	}
	want := []string{
		"MAM-GRAPH-0204 NodeFile web/file/etc/app/app.conf: /etc/app/app.conf restarts service app on web, which the declaration does not have there: it is restarted only if it exists and runs",
		"MAM-GRAPH-0203 NodeFile web/file/etc/app/key: the content of /etc/app/key looks like a private key: it would be in clear text in the declaration, the plan (and a pull request's comment), the Control Plane's database and the node's revision",
		"MAM-GRAPH-0203 NodeFile web/file/etc/app/token: the content of /etc/app/token looks like a GitHub token: it would be in clear text in the declaration, the plan (and a pull request's comment), the Control Plane's database and the node's revision",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
