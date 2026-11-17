package linux

import (
	"errors"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

const (
	exporterURL     = "https://github.com/prometheus/node_exporter/releases/download/v1.8.2/node_exporter-1.8.2.linux-amd64.tar.gz"
	exporterOldURL  = "https://github.com/prometheus/node_exporter/releases/download/v1.8.1/node_exporter-1.8.1.linux-amd64.tar.gz"
	exporterMember  = "node_exporter-1.8.2.linux-amd64/node_exporter"
	exporterOldMemb = "node_exporter-1.8.1.linux-amd64/node_exporter"
)

var (
	sumA = strings.Repeat("a", 64)
	sumB = strings.Repeat("b", 64)
)

func artifactRes(name, spec string) string {
	return `{"kind":"NodeArtifact","name":"` + name + `","spec":` + spec + `}`
}

// The defaults fill what is left out, and the canonical form writes them: two declarations of the
// same artifact are the same graph.
func TestArtifactDefaultsAndCanonicalForm(t *testing.T) {
	short, err := decodeGraph(artifactRes("web/artifact/usr/local/bin/tool", `{"node":"web","url":"https://example.com/tool","sha256":"`+sumA+`","path":"/usr/local/bin/tool"}`))
	if err != nil {
		t.Fatal(err)
	}
	long, err := decodeGraph(artifactRes("web/artifact/usr/local/bin/tool", `{"node":"web","url":"https://example.com/tool","sha256":"`+sumA+`","path":"/usr/local/bin/tool","mode":"0755","owner":"root","group":"root","restart":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if short.Digest() != long.Digest() {
		t.Errorf("the defaults make another graph:\n%s\n%s", short.Canonical(), long.Canonical())
	}
	want := `{"kind":"NodeArtifact","name":"web/artifact/usr/local/bin/tool","spec":{"group":"root","mode":"0755","node":"web","owner":"root","path":"/usr/local/bin/tool","sha256":"` + sumA + `","url":"https://example.com/tool"}}`
	if !strings.Contains(string(short.Canonical()), want) {
		t.Errorf("canonical %s", short.Canonical())
	}
	a := ArtifactOf(short.Resources[1])
	if a == nil || a.Mode != "0755" || a.Owner != "root" || a.Group != "root" || a.Extract != "" || a.Restart != nil || short.Resources[1].Node() != "web" {
		t.Errorf("%+v", a)
	}
	// With a member: it is in the canonical form.
	g, err := decodeGraph(artifactRes("web/artifact/usr/local/bin/node_exporter", `{"node":"web","url":"`+exporterURL+`","sha256":"`+sumA+`","path":"/usr/local/bin/node_exporter","extract":"`+exporterMember+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(g.Canonical()), `"spec":{"extract":"`+exporterMember+`","group":"root"`) {
		t.Errorf("canonical %s", g.Canonical())
	}
	// The kinds keep their places: an artifact comes before a file (an apply installs a binary before
	// the unit that runs it is written).
	both, err := decodeGraph(fileRes("web/file/etc/a", `{"node":"web","path":"/etc/a","content":""}`),
		artifactRes("web/artifact/usr/local/bin/tool", `{"node":"web","url":"https://example.com/tool","sha256":"`+sumA+`","path":"/usr/local/bin/tool"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := both.Sorted(); s[1].Kind != KindArtifact || s[2].Kind != KindFile {
		t.Errorf("order %v", s)
	}
	if d, _ := graph.KindOf(KindArtifact); d.Order != 8 {
		t.Errorf("NodeArtifact is at %d", d.Order)
	}
}

// What a declaration of an artifact may not be, with the code each problem has.
func TestArtifactRejections(t *testing.T) {
	res := func(path, members string) string {
		return artifactRes("web/artifact"+path, `{"node":"web","path":"`+path+`",`+members+`}`)
	}
	ok := `"url":"https://example.com/tool.tar.gz","sha256":"` + sumA + `"`
	for _, c := range []struct {
		name, res string
		code      graph.Code
		says      string
	}{
		{"no url", res("/usr/local/bin/x", `"sha256":"`+sumA+`"`), graph.CodeMissingField, "spec.url"},
		{"no sha256", res("/usr/local/bin/x", `"url":"https://example.com/x"`), graph.CodeMissingField, "spec.sha256"},
		{"unknown member", res("/usr/local/bin/x", ok+`,"checksum":"x"`), graph.CodeUnknownField, "checksum"},
		{"null extract", res("/usr/local/bin/x", ok+`,"extract":null`), graph.CodeWrongType, "extract"},
		{"http", res("/usr/local/bin/x", `"url":"http://example.com/x","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "not https"},
		{"HTTP", res("/usr/local/bin/x", `"url":"HTTP://example.com/x","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "not https"},
		{"ftp", res("/usr/local/bin/x", `"url":"ftp://example.com/x","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "not an https URL"},
		{"user", res("/usr/local/bin/x", `"url":"https://me:secret@example.com/x","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "user or a password"},
		{"fragment", res("/usr/local/bin/x", `"url":"https://example.com/x#y","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "fragment"},
		{"space", res("/usr/local/bin/x", `"url":"https://example.com/a b","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "not an https URL"},
		{"no host", res("/usr/local/bin/x", `"url":"https:///x","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "with a host"},
		{"long url", res("/usr/local/bin/x", `"url":"https://example.com/`+strings.Repeat("a", MaxURL)+`","sha256":"`+sumA+`"`), graph.CodeInvalidValue, "at most 2048"},
		{"short sha256", res("/usr/local/bin/x", `"url":"https://example.com/x","sha256":"abc"`), graph.CodeInvalidValue, "not a sha256"},
		{"upper sha256", res("/usr/local/bin/x", `"url":"https://example.com/x","sha256":"`+strings.Repeat("A", 64)+`"`), graph.CodeInvalidValue, "lowercase"},
		{"absolute member", res("/usr/local/bin/x", ok+`,"extract":"/x"`), graph.CodeInvalidValue, `starts with "/"`},
		{"dot member", res("/usr/local/bin/x", ok+`,"extract":"./x"`), graph.CodeInvalidValue, "not a clean path"},
		{"dot dot member", res("/usr/local/bin/x", ok+`,"extract":"../x"`), graph.CodeInvalidValue, `".." component`},
		{"inner dot dot", res("/usr/local/bin/x", ok+`,"extract":"a/../x"`), graph.CodeInvalidValue, "not a clean path"},
		{"directory member", res("/usr/local/bin/x", ok+`,"extract":"a/"`), graph.CodeInvalidValue, "a directory"},
		{"odd member", res("/usr/local/bin/x", ok+`,"extract":"a b"`), graph.CodeInvalidValue, "a character other than"},
		{"relative path", artifactRes("web/artifactusr/x", `{"node":"web","path":"usr/x",`+ok+`}`), graph.CodeInvalidValue, "not an absolute path"},
		{"forbidden path", res("/usr/local/bin/k3s", ok), graph.CodeInvalidValue, "k3s binary"},
		{"mamelet", res("/usr/local/sbin/mamelet", ok), graph.CodeInvalidValue, "mamelet itself"},
		{"name", artifactRes("web/usr/local/bin/x", `{"node":"web","path":"/usr/local/bin/x",`+ok+`}`), graph.CodeNameMismatch, `"web/artifact/usr/local/bin/x"`},
		{"node", artifactRes("db/artifact/usr/local/bin/x", `{"node":"db","path":"/usr/local/bin/x",`+ok+`}`), graph.CodeDanglingNode, "db"},
		{"setuid", res("/usr/local/bin/x", ok+`,"mode":"4755"`), graph.CodeInvalidValue, "setuid"},
		{"world-writable", res("/usr/local/bin/x", ok+`,"mode":"0777"`), graph.CodeInvalidValue, "every user"},
		{"owner", res("/usr/local/bin/x", ok+`,"owner":"Root"`), graph.CodeInvalidValue, "user's name"},
		{"restart name", res("/usr/local/bin/x", ok+`,"restart":["-x"]`), graph.CodeInvalidValue, "service name"},
		{"restart null", res("/usr/local/bin/x", ok+`,"restart":null`), graph.CodeWrongType, "restart"},
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

// URLs Mamela downloads from.
func TestCheckURLAccepts(t *testing.T) {
	for _, u := range []string{exporterURL, "https://example.com:8443/a/b?version=1&arch=amd64", "https://[2001:db8::1]/x.tar.gz",
		"https://github.com/k3s-io/k3s/releases/download/v1.33.5%2Bk3s1/k3s"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// One node, one path: an artifact and a file are not declared at one path, nor one under the other.
func TestArtifactConflictsWithAFile(t *testing.T) {
	ok := `"url":"https://example.com/tool","sha256":"` + sumA + `"`
	_, err := decodeGraph(
		fileRes("web/file/usr/local/bin/tool", `{"node":"web","path":"/usr/local/bin/tool","content":"#!/bin/sh\n"}`),
		artifactRes("web/artifact/usr/local/bin/tool", `{"node":"web","path":"/usr/local/bin/tool",`+ok+`}`))
	if err == nil || !strings.Contains(err.Error(), string(graph.CodeConflict)) || !strings.Contains(err.Error(), "also declared by NodeArtifact web/artifact/usr/local/bin/tool") {
		t.Errorf("a file and an artifact at one path: %v", err)
	}
	_, err = decodeGraph(
		artifactRes("web/artifact/opt/tool", `{"node":"web","path":"/opt/tool",`+ok+`}`),
		fileRes("web/file/opt/tool/config", `{"node":"web","path":"/opt/tool/config","content":""}`))
	if err == nil || !strings.Contains(err.Error(), "file or a directory") {
		t.Errorf("an artifact above a file: %v", err)
	}
	// The same path on two nodes is two artifacts.
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm + `,` +
		`{"kind":"TestVM","name":"db","provider":"test","spec":{"network":{"mode":"dhcp"}}},` +
		artifactRes("web/artifact/opt/tool", `{"node":"web","path":"/opt/tool",`+ok+`}`) + `,` +
		artifactRes("db/artifact/opt/tool", `{"node":"db","path":"/opt/tool",`+ok+`}`) + `]}`))
	if err != nil || len(g.Resources) != 4 {
		t.Errorf("two nodes: %v", err)
	}
}

// An artifact that restarts a service the declaration does not have on the node is warned about.
func TestArtifactWarnings(t *testing.T) {
	g, err := decodeGraph(
		artifactRes("web/artifact/usr/local/bin/node_exporter", `{"node":"web","path":"/usr/local/bin/node_exporter","url":"`+exporterURL+`","sha256":"`+sumA+`","extract":"`+exporterMember+`","restart":["node_exporter","prometheus"]}`),
		`{"kind":"NodeService","name":"web/node_exporter","spec":{"node":"web","service":"node_exporter","enabled":true,"running":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range g.Warnings() {
		got = append(got, string(w.Code)+" "+w.Resource+": "+w.Message)
	}
	want := "MAM-GRAPH-0215 NodeArtifact web/artifact/usr/local/bin/node_exporter: /usr/local/bin/node_exporter restarts service prometheus on web, which the declaration does not have there: it is restarted only if it exists and runs"
	if strings.Join(got, "\n") != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
}
