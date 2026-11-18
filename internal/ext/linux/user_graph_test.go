package linux

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// Public keys made for these tests (ssh-keygen; the private keys were thrown away), with the
// fingerprints ssh-keygen -l shows.
const (
	keyAlice   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOBGcoPBK3Riun0XLEh52zZlwGQmYj/pZrt1RXYeZ543 alice@laptop"
	fpAlice    = "SHA256:qH4TdBBALJZE6kzSMDYbGormDVp8hza5+wXLGeZ2Wws"
	keyBob     = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBJzdRIXtlqcMpmyaYnT5thVCJvnJTJwVNpLE5vSZX/UaAPn9UpGBnXX3GHxku1pur+0eWkSILgs7S10vCDa5r1k= bob@desk"
	fpBob      = "SHA256:VcWOyMmaA9ijf1osrgmDcsRk9kvDZebfo0Ym/X0FSD0"
	keyOldCI   = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDV4P/dRHfbrDT03/3FXU+1+rRB+34X3cqvDAgyWCypqdGLvJ77oC1J13cgK90JqblKe7zVKlPDNkqhTLGJnSxRIsls4kfBioRB2Wks+ZWuwKWXKygI8oG+Ovlq6UJhoZa/PGp+cMZ1Y4bMi8NpMKKql8vsaMPzJZ7lmocWYUO161AZnTvgKfC5FCSUXurpXPCtKiWkKUsB2EQqUQ4XU8C7e1Hm+W5a9uxUcYq/XUhih/MK0Il1oZbUBfD46Wc1ZiOQIjLh1PgumeCDK/cxbRYQwZX7YN9T6mUxpUBI0W73Rh29WGB0A/bmWzUjyXaOzSv6MJH/3VaX4+HwqR+NrSYf old-ci"
	fpOldCI    = "SHA256:CwCuVUW8tEA5ctpNdxJ+chpQlqFWwp3j7gM3xJtOUL8"
	alicePlain = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOBGcoPBK3Riun0XLEh52zZlwGQmYj/pZrt1RXYeZ543"
)

func userRes(name, spec string) string {
	return `{"kind":"NodeUser","name":"` + name + `","spec":` + spec + `}`
}

// jsonString is s as a JSON string (the keys have no character that needs escaping but "/").
func jsonString(s string) string { return `"` + s + `"` }

// The key lines a declaration may write, and what the plan shows of each.
func TestParseAuthorizedKey(t *testing.T) {
	for _, c := range []struct{ line, typ, fp, comment string }{
		{keyAlice, "ssh-ed25519", fpAlice, "alice@laptop"},
		{keyBob, "ecdsa-sha2-nistp256", fpBob, "bob@desk"},
		{keyOldCI, "ssh-rsa", fpOldCI, "old-ci"},
		{alicePlain, "ssh-ed25519", fpAlice, ""},
		{alicePlain + " a comment with spaces", "ssh-ed25519", fpAlice, "a comment with spaces"},
	} {
		k, err := ParseAuthorizedKey(c.line)
		if err != nil || k.Type != c.typ || k.Fingerprint != c.fp || k.Comment != c.comment {
			t.Errorf("%.30s: %+v %v", c.line, k, err)
		}
	}
	if s := (AuthorizedKey{Type: "ssh-ed25519", Fingerprint: fpAlice, Comment: "alice@laptop"}).String(); s != "ssh-ed25519 "+fpAlice+" alice@laptop" {
		t.Errorf("%s", s)
	}
	b64 := strings.Fields(keyAlice)[1]
	for name, c := range map[string]struct{ line, says string }{
		"empty":           {"", "empty"},
		"options":         {`command="/bin/true" ` + keyAlice, "options before the key"},
		"restrict":        {"restrict " + keyAlice, "options before the key"},
		"no-pty,from":     {`no-pty,from="10.0.0.1" ` + keyAlice, "options before the key"},
		"unknown type":    {"ssh-dss " + b64, "not a key type"},
		"certificate":     {"ssh-ed25519-cert-v01@openssh.com " + b64, "not a key type"},
		"type mismatch":   {"ssh-rsa " + b64 + " x", "does not decode as a ssh-rsa key"},
		"not base64":      {"ssh-ed25519 AAAA!!!! x", "not base64"},
		"truncated":       {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5", "does not decode"},
		"no key":          {"ssh-ed25519", "no key"},
		"two spaces":      {"ssh-ed25519  " + b64, "no key"},
		"leading space":   {" " + keyAlice, "starts or ends"},
		"trailing space":  {keyAlice + " ", "starts or ends"},
		"comment spaces":  {alicePlain + "  x", "more than one space"},
		"tab":             {"ssh-ed25519\t" + b64, "tab"},
		"newline":         {keyAlice + "\n" + keyBob, "newline"},
		"carriage return": {keyAlice + "\r", "carriage return"},
		"bidi":            {alicePlain + " a‮b", "right-to-left override"},
		"too long":        {alicePlain + " " + strings.Repeat("c", MaxKeyLine), "at most"},
	} {
		if _, err := ParseAuthorizedKey(c.line); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.says)
		}
	}
}

// What mamelet finds in a file it did not write: keys with options too, by fingerprint, and the
// lines it cannot read counted.
func TestScanAuthorizedKeys(t *testing.T) {
	content := "# a comment\n\n" + keyAlice + "\n" +
		`command="echo \"a b\"",no-pty ` + keyBob + "\n" +
		"ssh-dss AAAAB3NzaC1kc3MAAACBAP x\n" +
		"  " + keyOldCI + "  \n" +
		"garbage\n"
	keys, other := scanAuthorizedKeys(content)
	want := []FoundKey{{Type: "ssh-ed25519", Fingerprint: fpAlice}, {Type: "ecdsa-sha2-nistp256", Fingerprint: fpBob, Options: true}, {Type: "ssh-rsa", Fingerprint: fpOldCI}}
	if len(keys) != len(want) || other != 2 {
		t.Fatalf("keys %+v, other %d", keys, other)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, keys[i], want[i])
		}
	}
}

// The defaults fill nothing in: a member left out is a member not declared (Mamela does not change
// the shell of a user that exists unless it is declared), and the canonical form writes only what is.
func TestUserDecodeAndCanonicalForm(t *testing.T) {
	g, err := decodeGraph(userRes("web/user/app", `{"node":"web","name":"app"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"NodeUser","name":"web/user/app","spec":{"name":"app","node":"web","system":false}}`
	if !strings.Contains(string(g.Canonical()), want) {
		t.Errorf("canonical %s", g.Canonical())
	}
	u := UserOf(g.Resources[1])
	if u == nil || u.System || u.UID != 0 || u.Shell != "" || u.Home != "" || u.Groups != nil || u.AuthorizedKeys != nil || g.Resources[1].Node() != "web" {
		t.Fatalf("%+v", u)
	}
	if u.CreateShell() != DefaultShell {
		t.Errorf("shell %s", u.CreateShell())
	}
	if h, made := u.CreateHome(); h != "/home/app" || !made {
		t.Errorf("home %s %v", h, made)
	}
	sys := &UserSpec{Name: "svc", System: true}
	if h, made := sys.CreateHome(); h != "" || made || sys.CreateShell() != DefaultSystemShell {
		t.Errorf("a system user: %q %v %s", h, made, sys.CreateShell())
	}
	sys.AuthorizedKeys = []string{keyAlice}
	if _, made := sys.CreateHome(); !made {
		t.Error("a system user with keys gets no home for them")
	}
	full, err := decodeGraph(userRes("web/user/deploy", `{"node":"web","name":"deploy","system":true,"uid":1500,"shell":"/bin/sh","home":"/srv/deploy","groups":["www-data","adm"],"authorizedKeys":[`+jsonString(keyAlice)+`,`+jsonString(keyBob)+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"kind":"NodeUser","name":"web/user/deploy","spec":{"authorizedKeys":["` + keyAlice + `","` + keyBob + `"],"groups":["www-data","adm"],"home":"/srv/deploy","name":"deploy","node":"web","shell":"/bin/sh","system":true,"uid":1500}}`
	if !strings.Contains(string(full.Canonical()), want) {
		t.Errorf("canonical %s\nwant %s", full.Canonical(), want)
	}
	if d := UserOf(full.Resources[1]); d.KeysContent() != keyAlice+"\n"+keyBob+"\n" {
		t.Errorf("content %q", d.KeysContent())
	}
	// Before NodeFile: the users are made before the files they own are written.
	both, err := decodeGraph(fileRes("web/file/etc/a", `{"node":"web","path":"/etc/a","content":""}`), userRes("web/user/app", `{"node":"web","name":"app"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := both.Sorted(); s[1].Kind != KindUser || s[2].Kind != KindFile {
		t.Errorf("order %v", s)
	}
	if d, _ := graph.KindOf(KindUser); d.Order != 6 {
		t.Errorf("NodeUser is at %d", d.Order)
	}
}

// What a declaration of a user may not be, with the code each problem has.
func TestUserRejections(t *testing.T) {
	key := func(lines ...string) string {
		for i, l := range lines {
			lines[i] = jsonString(l)
		}
		return `{"node":"web","name":"app","authorizedKeys":[` + strings.Join(lines, ",") + `]}`
	}
	for _, c := range []struct {
		name, res string
		code      graph.Code
		says      string
	}{
		{"no name", userRes("web/user/app", `{"node":"web"}`), graph.CodeMissingField, "spec.name"},
		{"unknown member", userRes("web/user/app", `{"node":"web","name":"app","sudo":true}`), graph.CodeUnknownField, "sudo"},
		{"root", userRes("web/user/root", `{"node":"web","name":"root"}`), graph.CodeInvalidValue, "never manages root"},
		{"uid 0", userRes("web/user/app", `{"node":"web","name":"app","uid":0}`), graph.CodeInvalidValue, "uid 0 is root's"},
		{"negative uid", userRes("web/user/app", `{"node":"web","name":"app","uid":-1}`), graph.CodeWrongType, "not a positive integer"},
		{"float uid", userRes("web/user/app", `{"node":"web","name":"app","uid":1.5}`), graph.CodeWrongType, "not a positive integer"},
		{"exponent uid", userRes("web/user/app", `{"node":"web","name":"app","uid":1e3}`), graph.CodeWrongType, "not a positive integer"},
		{"string uid", userRes("web/user/app", `{"node":"web","name":"app","uid":"1001"}`), graph.CodeWrongType, "not a positive integer"},
		{"huge uid", userRes("web/user/app", `{"node":"web","name":"app","uid":4294967295}`), graph.CodeInvalidValue, "larger than"},
		{"system null", userRes("web/user/app", `{"node":"web","name":"app","system":null}`), graph.CodeWrongType, "system"},
		{"system string", userRes("web/user/app", `{"node":"web","name":"app","system":"yes"}`), graph.CodeWrongType, "system"},
		{"bad name", userRes("web/user/App", `{"node":"web","name":"App"}`), graph.CodeInvalidValue, "not a user's name"},
		{"option name", userRes("web/user/-o", `{"node":"web","name":"-o"}`), graph.CodeInvalidValue, "not a user's name"},
		{"name", userRes("web/app", `{"node":"web","name":"app"}`), graph.CodeNameMismatch, `"web/user/app"`},
		{"node", userRes("db/user/app", `{"node":"db","name":"app"}`), graph.CodeDanglingNode, "db"},
		{"relative shell", userRes("web/user/app", `{"node":"web","name":"app","shell":"bash"}`), graph.CodeInvalidValue, "not an absolute path"},
		{"shell with space", userRes("web/user/app", `{"node":"web","name":"app","shell":"/bin/ba sh"}`), graph.CodeInvalidValue, "component"},
		{"null shell", userRes("web/user/app", `{"node":"web","name":"app","shell":null}`), graph.CodeWrongType, "shell"},
		{"home /", userRes("web/user/app", `{"node":"web","name":"app","home":"/"}`), graph.CodeInvalidValue, `is "/"`},
		{"home right under /", userRes("web/user/app", `{"node":"web","name":"app","home":"/srv"}`), graph.CodeInvalidValue, "right under /"},
		{"home in /etc", userRes("web/user/app", `{"node":"web","name":"app","home":"/etc/app"}`), graph.CodeInvalidValue, "the system's"},
		{"home in /tmp", userRes("web/user/app", `{"node":"web","name":"app","home":"/tmp/app"}`), graph.CodeInvalidValue, "the system's"},
		{"home /root", userRes("web/user/app", `{"node":"web","name":"app","home":"/root"}`), graph.CodeInvalidValue, "/root"},
		{"home dot dot", userRes("web/user/app", `{"node":"web","name":"app","home":"/home/../etc"}`), graph.CodeInvalidValue, "not a clean path"},
		{"group name", userRes("web/user/app", `{"node":"web","name":"app","groups":["Docker"]}`), graph.CodeInvalidValue, "group's name"},
		{"group twice", userRes("web/user/app", `{"node":"web","name":"app","groups":["docker","docker"]}`), graph.CodeInvalidValue, "twice"},
		{"groups null", userRes("web/user/app", `{"node":"web","name":"app","groups":null}`), graph.CodeWrongType, "groups"},
		{"groups not a list", userRes("web/user/app", `{"node":"web","name":"app","groups":"docker"}`), graph.CodeWrongType, "groups"},
		{"keys null", userRes("web/user/app", `{"node":"web","name":"app","authorizedKeys":null}`), graph.CodeWrongType, "authorizedKeys"},
		{"keys empty", userRes("web/user/app", `{"node":"web","name":"app","authorizedKeys":[]}`), graph.CodeInvalidValue, "leave the member out"},
		{"keys not strings", userRes("web/user/app", `{"node":"web","name":"app","authorizedKeys":[1]}`), graph.CodeWrongType, "authorizedKeys"},
		{"bad key line", userRes("web/user/app", key(keyAlice, "ssh-ed25519 AAAA x")), graph.CodeInvalidValue, "spec.authorizedKeys[1]"},
		{"options", userRes("web/user/app", key(`from=\"10.0.0.0/8\" `+keyAlice)), graph.CodeInvalidValue, "options before the key"},
		{"key twice", userRes("web/user/app", key(keyAlice, alicePlain+" other comment")), graph.CodeInvalidValue, "also line 0"},
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

// One node: a user is declared once, and two users do not share a uid or a home.
func TestUserConflicts(t *testing.T) {
	_, err := decodeGraph(userRes("web/user/app", `{"node":"web","name":"app"}`), userRes("web/user/app", `{"node":"web","name":"app","system":true}`))
	if err == nil || !strings.Contains(err.Error(), string(graph.CodeDuplicateResource)) {
		t.Errorf("one user twice: %v", err)
	}
	_, err = decodeGraph(userRes("web/user/a", `{"node":"web","name":"a","uid":1500}`), userRes("web/user/b", `{"node":"web","name":"b","uid":1500}`))
	if err == nil || !strings.Contains(err.Error(), string(graph.CodeConflict)) || !strings.Contains(err.Error(), "uid 1500 is also declared by NodeUser web/user/a") {
		t.Errorf("one uid twice: %v", err)
	}
	_, err = decodeGraph(userRes("web/user/a", `{"node":"web","name":"a","home":"/home/b"}`), userRes("web/user/b", `{"node":"web","name":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "home /home/b is also the home of NodeUser web/user/a") {
		t.Errorf("one home twice: %v", err)
	}
	// The same user on two nodes is two users.
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm + `,` +
		`{"kind":"TestVM","name":"db","provider":"test","spec":{"network":{"mode":"dhcp"}}},` +
		userRes("web/user/app", `{"node":"web","name":"app","uid":1500}`) + `,` +
		userRes("db/user/app", `{"node":"db","name":"app","uid":1500}`) + `]}`))
	if err != nil || len(g.Resources) != 4 {
		t.Errorf("two nodes: %v", err)
	}
}

// madeUpKey is an ed25519 public key line of key i (not a real key: the graph checks the form and
// the embedded type, not the curve).
func madeUpKey(i int, comment string) string {
	blob := binary.BigEndian.AppendUint32(nil, 11)
	blob = append(blob, "ssh-ed25519"...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	point := sha256.Sum256([]byte(strconv.Itoa(i)))
	blob = append(blob, point[:]...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
}

// The keys of a node's users ride in its revision: all of them together are bounded.
func TestUserKeysPerNode(t *testing.T) {
	var users []string
	n := 0
	for u := range MaxNodeContent/(30*8000) + 1 {
		var lines []string
		for range 30 {
			lines = append(lines, jsonString(madeUpKey(n, strings.Repeat("c", 8000))))
			n++
		}
		name := fmt.Sprintf("u%d", u)
		users = append(users, userRes("web/user/"+name, `{"node":"web","name":"`+name+`","authorizedKeys":[`+strings.Join(lines, ",")+`]}`))
	}
	_, err := decodeGraph(users...)
	if err == nil || !strings.Contains(err.Error(), "hold more than 1048576 bytes") {
		t.Errorf("%.300v", err)
	}
	if _, err := decodeGraph(users[:len(users)-1]...); err != nil {
		t.Errorf("within the bound: %.300v", err)
	}
}

// A group that lets its members become root is said, with the user.
func TestUserWarnings(t *testing.T) {
	g, err := decodeGraph(userRes("web/user/app", `{"node":"web","name":"app","groups":["www-data","docker","sudo"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range g.Warnings() {
		got = append(got, string(w.Code)+" "+w.Resource+": "+w.Message)
	}
	want := []string{
		"MAM-GRAPH-0206 NodeUser web/user/app: user app is added to group docker on web: its members control the Docker daemon, which runs containers as root with the host's files, so the user can become root there",
		"MAM-GRAPH-0206 NodeUser web/user/app: user app is added to group sudo on web: its members control sudo, so the user can become root there",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The homes Mamela makes or writes keys into.
func TestCheckHome(t *testing.T) {
	for _, ok := range []string{"/home/app", "/srv/app", "/var/lib/postgresql", "/var/www", "/opt/app/home"} {
		if e := CheckHome(ok); e != nil {
			t.Errorf("%s: %v", ok, e)
		}
	}
	for _, bad := range []string{"", "home/app", "/", "/home", "/nonexistent", "/etc/app", "/root", "/tmp/x", "/var/tmp/x", "/usr/local/app",
		"/run/sshd", "/proc/1", "/var/lib/mamelet/x", "/home/app/", "/home//app", "/home/a b"} {
		if e := CheckHome(bad); e == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
