package linux

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

func aptRes(name, spec string) string {
	return `{"kind":"NodeAptRepo","name":"` + name + `","spec":` + spec + `}`
}

// The sha256 of the test's keys (testKeyArmored, testKeyBinary in aptrepo_mamelet_test.go).
var (
	armoredSHA = ContentSHA256(testKeyArmored)
	binarySHA  = ContentSHA256(testKeyBinary)
)

// dockerSpec is Docker's source for Ubuntu 24.04, with the test's key.
func dockerSpec(more string) string {
	return `{"node":"web","name":"docker","uris":["https://download.docker.com/linux/ubuntu"],"suites":["noble"],"components":["stable"],` +
		`"keyUrl":"https://download.docker.com/linux/ubuntu/gpg","keySha256":"` + armoredSHA + `"` + more + `}`
}

var dockerRepo = aptRes("web/apt-repo/docker", dockerSpec(""))

// The canonical form writes every member but an empty architectures; the kind sorts after the
// providers' and before NodeFile.
func TestAptRepoCanonicalForm(t *testing.T) {
	short, err := decodeGraph(dockerRepo)
	if err != nil {
		t.Fatal(err)
	}
	long, err := decodeGraph(aptRes("web/apt-repo/docker", dockerSpec(`,"architectures":[]`)))
	if err != nil {
		t.Fatal(err)
	}
	if short.Digest() != long.Digest() {
		t.Errorf("an empty architectures makes another graph:\n%s\n%s", short.Canonical(), long.Canonical())
	}
	want := `{"kind":"NodeAptRepo","name":"web/apt-repo/docker","spec":{"components":["stable"],"keySha256":"` + armoredSHA +
		`","keyUrl":"https://download.docker.com/linux/ubuntu/gpg","name":"docker","node":"web","suites":["noble"],"uris":["https://download.docker.com/linux/ubuntu"]}}`
	if !strings.Contains(string(short.Canonical()), want) {
		t.Errorf("canonical %s", short.Canonical())
	}
	a := AptRepoOf(short.Resources[1])
	if a == nil || a.Architectures != nil || short.Resources[1].Node() != "web" {
		t.Errorf("%+v", a)
	}
	flat, err := decodeGraph(aptRes("web/apt-repo/flat", `{"node":"web","name":"flat","uris":["http://deb.example.org/repo"],"suites":["./"],"components":[],"keyUrl":"https://deb.example.org/key.asc","keySha256":"`+armoredSHA+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(flat.Canonical()), `"components":[]`) {
		t.Errorf("a flat repository's components: %s", flat.Canonical())
	}
	g, err := decodeGraph(fileRes("web/file/etc/motd", `{"node":"web","path":"/etc/motd","content":"hi\n"}`), dockerRepo)
	if err != nil {
		t.Fatal(err)
	}
	if s := g.Sorted(); s[1].Kind != KindAptRepo || s[2].Kind != KindFile {
		t.Errorf("order %v", s)
	}
	if d, _ := graph.KindOf(KindAptRepo); d.Order != 7 {
		t.Errorf("NodeAptRepo is at %d", d.Order)
	}
}

// What a declaration of a source may not be. Every value goes into a deb822 file as it is, so none
// may carry a newline, a space or a control character: nothing can add a line (a Signed-By of its
// own, an option) to the file.
func TestAptRepoRejections(t *testing.T) {
	spec := func(field, value string) string {
		m := map[string]string{
			"node": `"web"`, "name": `"docker"`, "uris": `["https://download.docker.com/linux/ubuntu"]`, "suites": `["noble"]`,
			"components": `["stable"]`, "keyUrl": `"https://download.docker.com/linux/ubuntu/gpg"`, "keySha256": `"` + armoredSHA + `"`,
		}
		if value == "" {
			delete(m, field)
		} else {
			m[field] = value
		}
		var parts []string
		for _, k := range []string{"node", "name", "uris", "suites", "components", "architectures", "keyUrl", "keySha256", "extra"} {
			if v, ok := m[k]; ok {
				parts = append(parts, `"`+k+`":`+v)
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	repo := func(field, value string) string { return aptRes("web/apt-repo/docker", spec(field, value)) }
	for _, c := range []struct {
		name, res string
		code      graph.Code
		says      string
	}{
		{"no key sha256", repo("keySha256", ""), graph.CodeMissingField, "spec.keySha256"},
		{"no components", repo("components", ""), graph.CodeMissingField, "spec.components"},
		{"unknown member", repo("extra", `"x"`), graph.CodeUnknownField, "extra"},
		{"null architectures", repo("architectures", `null`), graph.CodeWrongType, "architectures"},
		{"architectures not a list", repo("architectures", `"amd64"`), graph.CodeWrongType, "architectures"},
		{"name", aptRes("web/apt-repo/Docker", spec("name", `"Docker"`)), graph.CodeInvalidValue, "not the name of an APT source"},
		{"name with a path", aptRes("web/apt-repo/../x", spec("name", `"../x"`)), graph.CodeInvalidValue, "not the name of an APT source"},
		{"name mismatch", aptRes("web/docker", spec("", "")), graph.CodeNameMismatch, `"web/apt-repo/docker"`},
		{"node", aptRes("db/apt-repo/docker", spec("node", `"db"`)), graph.CodeDanglingNode, "db"},
		{"no URI", repo("uris", `[]`), graph.CodeInvalidValue, "there is no URI"},
		{"five URIs", repo("uris", `["http://a.example/1","http://a.example/2","http://a.example/3","http://a.example/4","http://a.example/5"]`), graph.CodeInvalidValue, "at most 4"},
		{"URI twice", repo("uris", `["http://a.example/1","http://a.example/1"]`), graph.CodeInvalidValue, "named twice"},
		{"ftp", repo("uris", `["ftp://a.example/debian"]`), graph.CodeInvalidValue, "not an http or https URI"},
		{"file", repo("uris", `["file:/srv/repo"]`), graph.CodeInvalidValue, "not an http or https URI"},
		{"mirror+", repo("uris", `["mirror+http://a.example/list"]`), graph.CodeInvalidValue, "not an http or https URI"},
		{"credentials", repo("uris", `["https://user:secret@a.example/debian"]`), graph.CodeInvalidValue, "user name or a password"},
		{"query", repo("uris", `["https://a.example/debian?x=1"]`), graph.CodeInvalidValue, "query or a fragment"},
		{"fragment", repo("uris", `["https://a.example/debian#x"]`), graph.CodeInvalidValue, "query or a fragment"},
		{"newline in a URI", repo("uris", `["https://a.example/debian\nSigned-By: /etc/apt/keyrings/evil.gpg"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"space in a URI", repo("uris", `["https://a.example/debian [trusted=yes]"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"dots in a URI", repo("uris", `["https://a.example/debian/../x"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"IPv6", repo("uris", `["http://[2001:db8::1]/debian"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"port", repo("uris", `["http://a.example:70000/debian"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"variable", repo("uris", `["http://a.example/$(ARCH)"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"bidi in a URI", repo("uris", `["https://a.example/‮x"]`), graph.CodeInvalidValue, "not a URI Mamela writes"},
		{"no suite", repo("suites", `[]`), graph.CodeInvalidValue, "there is no suite"},
		{"newline in a suite", repo("suites", `["noble\nTypes: deb-src"]`), graph.CodeInvalidValue, "not a suite"},
		{"space in a suite", repo("suites", `["noble main"]`), graph.CodeInvalidValue, "not a suite"},
		{"dot dot suite", repo("suites", `["../../x"]`), graph.CodeInvalidValue, "not a suite"},
		{"suite twice", repo("suites", `["noble","noble"]`), graph.CodeInvalidValue, "named twice"},
		{"newline in a component", repo("components", `["stable\nSigned-By: /x"]`), graph.CodeInvalidValue, "not a component"},
		{"option as a component", repo("components", `["[trusted=yes]"]`), graph.CodeInvalidValue, "not a component"},
		{"no component", repo("components", `[]`), graph.CodeInvalidValue, "only a flat repository"},
		{"flat with components", repo("suites", `["./"]`), graph.CodeInvalidValue, "has no components"},
		{"flat and not", aptRes("web/apt-repo/docker", strings.Replace(spec("suites", `["./","noble"]`), `["stable"]`, `[]`, 1)), graph.CodeInvalidValue, "some suites end"},
		{"architecture", repo("architectures", `["amd64\nTrusted: yes"]`), graph.CodeInvalidValue, "not a dpkg architecture"},
		{"architecture twice", repo("architectures", `["amd64","amd64"]`), graph.CodeInvalidValue, "named twice"},
		{"http key", repo("keyUrl", `"http://download.docker.com/linux/ubuntu/gpg"`), graph.CodeInvalidValue, "not an https URL"},
		{"newline in the key URL", repo("keyUrl", `"https://a.example/key\nx"`), graph.CodeInvalidValue, "not a URL Mamela downloads"},
		{"key URL credentials", repo("keyUrl", `"https://u:p@a.example/key"`), graph.CodeInvalidValue, "user name or a password"},
		{"key URL fragment", repo("keyUrl", `"https://a.example/key#x"`), graph.CodeInvalidValue, "fragment"},
		{"key sha256", repo("keySha256", `"`+strings.ToUpper(armoredSHA)+`"`), graph.CodeInvalidValue, "not a sha256"},
		{"short key sha256", repo("keySha256", `"abc"`), graph.CodeInvalidValue, "not a sha256"},
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

// What Mamela's Pome writes (pome/mamela/test/graph_test.emel) is the canonical form, byte for byte.
func TestAptRepoFromThePome(t *testing.T) {
	for _, pome := range []string{
		`{"kind":"NodeAptRepo","name":"web/apt-repo/docker","spec":{"components":["stable"],"keySha256":"0000000000000000000000000000000000000000000000000000000000000000","keyUrl":"https://download.docker.com/linux/ubuntu/gpg","name":"docker","node":"web","suites":["noble"],"uris":["https://download.docker.com/linux/ubuntu"]}}`,
		`{"kind":"NodeAptRepo","name":"web/apt-repo/flat","spec":{"architectures":["amd64","arm64"],"components":[],"keySha256":"1111111111111111111111111111111111111111111111111111111111111111","keyUrl":"https://deb.example.org/key.asc","name":"flat","node":"web","suites":["./"],"uris":["http://deb.example.org/repo","https://mirror.example.org/repo"]}}`,
	} {
		g, err := decodeGraph(pome)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(g.Canonical()), pome) {
			t.Errorf("canonical %s\nthe Pome's %s", g.Canonical(), pome)
		}
	}
}

// What a declaration may be: a flat repository, several URIs and suites, a port, a key server.
func TestAptRepoAccepted(t *testing.T) {
	for name, spec := range map[string]string{
		"flat":       `{"node":"web","name":"flat","uris":["http://deb.example.org/repo"],"suites":["./"],"components":[],"keyUrl":"https://deb.example.org/key.asc","keySha256":"` + armoredSHA + `"}`,
		"root flat":  `{"node":"web","name":"flat","uris":["http://deb.example.org/repo"],"suites":["/"],"components":[],"keyUrl":"https://deb.example.org/key.asc","keySha256":"` + armoredSHA + `"}`,
		"several":    `{"node":"web","name":"flat","uris":["https://a.example/debian","http://b.example:8080/debian/"],"suites":["bookworm","bookworm-updates"],"components":["main","contrib"],"architectures":["amd64","arm64"],"keyUrl":"https://a.example/key.gpg","keySha256":"` + armoredSHA + `"}`,
		"key server": `{"node":"web","name":"flat","uris":["https://ppa.launchpadcontent.net/deadsnakes/ppa/ubuntu"],"suites":["noble"],"components":["main"],"keyUrl":"https://keyserver.ubuntu.com/pks/lookup?op=get&search=0xF23C5A6CF475977595C89F51BA6932366A755776","keySha256":"` + armoredSHA + `"}`,
		"node%5f20":  `{"node":"web","name":"flat","uris":["https://deb.nodesource.com/node_20.x"],"suites":["nodistro"],"components":["main"],"keyUrl":"https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key","keySha256":"` + armoredSHA + `"}`,
	} {
		if _, err := decodeGraph(aptRes("web/apt-repo/flat", spec)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Two sources of one node must not name one URI and suite (apt would refuse them both); on two nodes
// they may.
func TestAptRepoConflicts(t *testing.T) {
	other := aptRes("web/apt-repo/docker2", strings.Replace(strings.Replace(dockerSpec(""), `"docker"`, `"docker2"`, 1),
		`"https://download.docker.com/linux/ubuntu"`, `"https://download.docker.com/linux/ubuntu/"`, 1))
	_, err := decodeGraph(dockerRepo, other)
	if err == nil || !strings.Contains(err.Error(), string(graph.CodeConflict)) || !strings.Contains(err.Error(), "apt refuses one source with two keys") {
		t.Errorf("one URI and suite twice: %v", err)
	}
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm + `,` +
		`{"kind":"TestVM","name":"db","provider":"test","spec":{"network":{"mode":"dhcp"}}},` + dockerRepo + `,` +
		aptRes("db/apt-repo/docker", strings.Replace(dockerSpec(""), `"node":"web"`, `"node":"db"`, 1)) + `]}`))
	if err != nil || len(g.Resources) != 4 {
		t.Errorf("two nodes: %v", err)
	}
}

// The file mamelet writes, as the plan shows it (one function renders it for both).
func TestAptSourcesRendering(t *testing.T) {
	docker := AptRepoSpec{Name: "docker", URIs: []string{"https://download.docker.com/linux/ubuntu"}, Suites: []string{"noble"},
		Components: []string{"stable"}, KeyURL: "https://download.docker.com/linux/ubuntu/gpg", KeySHA256: armoredSHA}
	golden(t, "apt/docker.sources", []byte(AptSources(docker, AptKeyPath("docker", true))))
	several := AptRepoSpec{Name: "debian", URIs: []string{"https://a.example/debian", "http://b.example:8080/debian/"}, Suites: []string{"bookworm", "bookworm-updates"},
		Components: []string{"main", "contrib"}, Architectures: []string{"amd64", "arm64"}, KeyURL: "https://a.example/key.gpg", KeySHA256: binarySHA}
	golden(t, "apt/several-binary-key.sources", []byte(AptSources(several, AptKeyPath("debian", false))))
	flat := AptRepoSpec{Name: "flat", URIs: []string{"http://deb.example.org/repo"}, Suites: []string{"./"}, Components: []string{},
		KeyURL: "https://deb.example.org/key.asc", KeySHA256: armoredSHA}
	golden(t, "apt/flat.sources", []byte(AptSources(flat, AptKeyPath("flat", true))))
	if a, b := aptSourcesSHA256(docker); a == b || a != ContentSHA256(AptSources(docker, "/etc/apt/keyrings/mamela-docker.asc")) {
		t.Errorf("the two forms: %s %s", a, b)
	}
	// Every line is a comment or one field; nothing else.
	for _, l := range strings.Split(strings.TrimSuffix(AptSources(several, AptKeyPath("debian", false)), "\n"), "\n") {
		k, _, ok := strings.Cut(l, ": ")
		if !strings.HasPrefix(l, "# ") && (!ok || !slices.Contains([]string{"Types", "URIs", "Suites", "Components", "Architectures", "Signed-By"}, k)) {
			t.Errorf("line %q", l)
		}
	}
}

// The names apt gives the release files of a source in /var/lib/apt/lists (as an apt-get update
// leaves them on Debian and Ubuntu).
func TestAptListsName(t *testing.T) {
	for _, c := range []struct{ uri, suite, want string }{
		{"https://download.docker.com/linux/ubuntu", "noble", "download.docker.com_linux_ubuntu_dists_noble_InRelease"},
		{"https://download.docker.com/linux/ubuntu/", "noble", "download.docker.com_linux_ubuntu_dists_noble_InRelease"},
		{"https://apt.grafana.com", "stable", "apt.grafana.com_dists_stable_InRelease"},
		{"http://archive.ubuntu.com/ubuntu", "noble-updates", "archive.ubuntu.com_ubuntu_dists_noble-updates_InRelease"},
		{"https://deb.nodesource.com/node_20.x", "nodistro", "deb.nodesource.com_node%5f20.x_dists_nodistro_InRelease"},
		{"http://deb.example.org/repo", "./", "deb.example.org_repo_._InRelease"},
		{"http://deb.example.org/repo", "/", "deb.example.org_repo_InRelease"},
		{"http://deb.example.org/repo", "stable/", "deb.example.org_repo_stable_InRelease"},
		{"http://localhost:8080/debian", "stable/updates", "localhost:8080_debian_dists_stable_updates_InRelease"},
		{"https://a.example/~user/debian", "x", "a.example_%7euser_debian_dists_x_InRelease"},
	} {
		if got := aptListsName(c.uri, c.suite, "InRelease"); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.uri, c.suite, got, c.want)
		}
	}
}

// The forms of a key apt reads, and what is refused.
func TestAptKeyForm(t *testing.T) {
	for name, c := range map[string]struct {
		key     string
		armored bool
		err     string
	}{
		"armored":            {testKeyArmored, true, ""},
		"armored after a CR": {"\r\n" + testKeyArmored, true, ""},
		"binary old format":  {testKeyBinary, false, ""},
		"binary new format":  {"\xc6\x33\x04", false, ""},
		"html":               {"<!DOCTYPE html><html><body>Not Found</body></html>", false, "not an OpenPGP public key"},
		"private key":        {"-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nxx\n-----END PGP PRIVATE KEY BLOCK-----\n", false, "not an OpenPGP public key"},
		"cut short":          {"-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBF", false, "without its end line"},
		"keybox":             {"\x00\x00\x00\x20\x01\x01\x00\x00KBXf", false, "not an OpenPGP public key"},
		"empty":              {"", false, "not an OpenPGP public key"},
	} {
		armored, err := aptKeyForm([]byte(c.key))
		switch {
		case c.err == "" && (err != nil || armored != c.armored):
			t.Errorf("%s: %v %v", name, armored, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v", name, err)
		}
	}
}

// What mamelet reads of the node's other source files, to find one that names the same URI and
// suite.
func TestAptSourceFiles(t *testing.T) {
	list := oneLineEntries("/etc/apt/sources.list.d/docker.list", "# Docker\n"+
		"deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n"+
		"deb [ arch=amd64 ] http://a.example/debian bookworm main # a comment\n"+
		"# deb https://commented.example/x y z\n"+
		"deb-src https://src.example/debian sid main\n"+
		"rubbish\n")
	want := []aptEntry{{"/etc/apt/sources.list.d/docker.list", "https://download.docker.com/linux/ubuntu", "noble"},
		{"/etc/apt/sources.list.d/docker.list", "http://a.example/debian", "bookworm"},
		{"/etc/apt/sources.list.d/docker.list", "https://src.example/debian", "sid"}}
	if !slices.Equal(list, want) {
		t.Errorf("one-line: %+v", list)
	}
	d := deb822Entries("/etc/apt/sources.list.d/ubuntu.sources", "## Ubuntu\nTypes: deb\nURIs: http://archive.ubuntu.com/ubuntu/\nSuites: noble noble-updates\n"+
		"Components: main\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n\n"+
		"Types: deb\nURIs: http://disabled.example/x\nSuites: y\nEnabled: no\n\n"+
		"types: deb\nuris:\n http://security.ubuntu.com/ubuntu/\nsuites: noble-security\n")
	want = []aptEntry{{"/etc/apt/sources.list.d/ubuntu.sources", "http://archive.ubuntu.com/ubuntu/", "noble"},
		{"/etc/apt/sources.list.d/ubuntu.sources", "http://archive.ubuntu.com/ubuntu/", "noble-updates"},
		{"/etc/apt/sources.list.d/ubuntu.sources", "http://security.ubuntu.com/ubuntu/", "noble-security"}}
	if !slices.Equal(d, want) {
		t.Errorf("deb822: %+v", d)
	}
	all := append(list, d...)
	all = append(all, aptEntry{"/etc/apt/sources.list.d/mamela-docker.sources", "https://download.docker.com/linux/ubuntu", "noble"})
	if c := aptConflicts(all, "/etc/apt/sources.list.d/mamela-docker.sources", []string{"https://download.docker.com/linux/ubuntu/"}, []string{"noble"}); !slices.Equal(c, []string{"/etc/apt/sources.list.d/docker.list"}) {
		t.Errorf("conflicts %v", c)
	}
	if c := aptConflicts(all, "/etc/apt/sources.list.d/mamela-docker.sources", []string{"https://download.docker.com/linux/ubuntu"}, []string{"jammy"}); len(c) != 0 {
		t.Errorf("another suite: %v", c)
	}
}
