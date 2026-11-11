package host

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// recorder is an API that records what it receives, uploads included.
type recorder struct {
	got   []string // "METHOD path form-or-multipart"
	files map[string]string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	line := req.Method + " " + req.URL.Path
	ct, params, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
	switch ct {
	case "multipart/form-data":
		mr := multipart.NewReader(req.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				r.files[p.FileName()] = string(b)
				line += " file:" + p.FormName() + "=" + p.FileName()
			} else {
				line += " " + p.FormName() + "=" + string(b)
			}
		}
		if req.ContentLength <= 0 {
			line += " (no length)"
		}
	default:
		req.ParseForm()
		if len(req.PostForm) > 0 {
			line += " " + req.PostForm.Encode()
		}
	}
	r.got = append(r.got, line)
	if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/vms") {
		w.WriteHeader(200)
		return
	}
	w.Write([]byte("{}"))
}

// maker makes artifacts in memory; seeds get a name and content only when made.
type maker struct{ made int }

func (m *maker) Make(_ context.Context, a protocol.Artifact) (Prepared, error) {
	m.made++
	content := "image bytes"
	name := a.Name
	if a.Kind == protocol.ArtifactSeed {
		content, name = "seed with token-"+a.Node, "seed-"+a.Node+"-1234.iso"
	}
	if a.Kind == protocol.ArtifactDownload && a.SHA256 == strings.Repeat("0", 64) {
		return Prepared{}, errors.New("sha256 mismatch")
	}
	return Prepared{Info: protocol.ArtifactInfo{Name: name, SHA256: strings.Repeat("a", 64), Size: int64(len(content))},
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }}, nil
}

var (
	uploadCall      = protocol.Call{Method: "POST", Path: "/storage/{storage}/upload", Form: []string{"content", "checksum"}, File: "filename"}
	createCall      = protocol.Call{Method: "POST", Path: "/vms", Form: []string{"vm", "cdrom"}}
	configCall      = protocol.Call{Method: "PUT", Path: "/vms/{vm}/config", Form: []string{"tags", "digest"}}
	artifactCatalog = protocol.Catalog{API: "test", Entries: []protocol.Entry{
		{Method: "POST", Path: "/storage/{storage}/upload", Effect: protocol.EffectCreate, Form: []string{"content", "checksum"}, File: "filename"},
		{Method: "POST", Path: "/vms", Effect: protocol.EffectCreate, Form: []string{"vm", "cdrom"}},
		{Method: "PUT", Path: "/vms/{vm}/config", Effect: protocol.EffectChange, Form: []string{"tags", "digest"}, Precondition: "digest"},
	}}
)

func artifactPermit(t *testing.T) *protocol.Permit {
	t.Helper()
	m := protocol.Manifest{Schema: protocol.Schema, Provider: "p", Version: "1", API: "test", Kinds: []protocol.Kind{{Kind: "VM", Node: true,
		Operations: []protocol.Operation{{Op: "create", Support: protocol.Supported, Calls: []protocol.Call{uploadCall, createCall, configCall}}}}}}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
	c := protocol.Change{Resource: "VM web", Kind: "VM", Op: "create", IDs: map[string]string{"vm": "7", "storage": "local"},
		Artifacts: []protocol.Artifact{{ID: "seed", Kind: protocol.ArtifactSeed, Node: "web", NodeID: 7, Network: &protocol.Network{Mode: "dhcp"}}},
		Requests: []protocol.Approved{
			{Method: "POST", Path: "/storage/{storage}/upload", Upload: "seed", Params: map[string][]string{"content": {"iso"}, "checksum": {"{artifact:seed.sha256}"}}},
			{Method: "POST", Path: "/vms", Params: map[string][]string{"vm": {"7"}, "cdrom": {"local:iso/{artifact:seed.name}"}}},
			{Method: "PUT", Path: "/vms/{vm}/config", Params: map[string][]string{"tags": {"lab"}}},
		}}
	p, err := protocol.NewPermit(m, c, "op", "plan", "providers", testNow().Add(time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

// The host makes the artifacts a change declares, sends an upload as multipart with the artifact
// as its file, fills the artifacts' facts into the approved values, and allows a change without a
// version only to the object this apply created.
func TestArtifactsAndUploads(t *testing.T) {
	api := &recorder{files: map[string]string{}}
	m := &maker{}
	p := artifactPermit(t)
	g := gate{phase: phaseApply, calls: p.Calls, permit: p, catalog: artifactCatalog, upstream: Handler{Handler: api}, now: testNow,
		used: make([]bool, len(p.Requests)), artifacts: m}
	ctx := context.Background()
	send := func(r protocol.HostRequest) *protocol.Error {
		_, perr := g.send(ctx, r)
		return perr
	}
	upload := protocol.HostRequest{Method: "POST", Path: "/storage/local/upload", Upload: "seed",
		Form: map[string][]string{"content": {"iso"}, "checksum": {strings.Repeat("a", 64)}}}
	if perr := send(upload); perr == nil || !strings.Contains(perr.Message, "not ready") {
		t.Fatalf("an upload before the artifact is ready: %v", perr)
	}
	g.halt = "" // a refusal halts the apply; start again as a fresh apply would
	if _, perr := g.artifact(ctx, "other"); perr == nil || perr.Code != protocol.CodeRefused {
		t.Fatalf("an artifact the permit does not name: %v", perr)
	}
	info, perr := g.artifact(ctx, "seed")
	if perr != nil || info.Name != "seed-web-1234.iso" || info.ID != "seed" {
		t.Fatalf("artifact: %+v %v", info, perr)
	}
	if again, _ := g.artifact(ctx, "seed"); again != info || m.made != 1 {
		t.Fatal("an artifact is made once per apply")
	}
	// The provider cannot send the file itself, nor the update before creating.
	cheat := upload
	cheat.Form = map[string][]string{"content": {"iso"}, "checksum": {strings.Repeat("a", 64)}, "filename": {"evil"}}
	if perr := send(cheat); perr == nil {
		t.Fatal("the provider sent the file parameter")
	}
	g.halt = ""
	if perr := send(protocol.HostRequest{Method: "PUT", Path: "/vms/7/config", Form: map[string][]string{"tags": {"lab"}}}); perr == nil ||
		!strings.Contains(perr.Message, "needs a precondition") {
		t.Fatalf("a change without a version before the object was created: %v", perr)
	}
	g.halt = ""
	if perr := send(upload); perr != nil {
		t.Fatal(perr)
	}
	if perr := send(protocol.HostRequest{Method: "POST", Path: "/vms", Form: map[string][]string{"vm": {"7"}, "cdrom": {"local:iso/seed-web-1234.iso"}}}); perr != nil {
		t.Fatal(perr)
	}
	if perr := send(protocol.HostRequest{Method: "PUT", Path: "/vms/7/config", Form: map[string][]string{"tags": {"lab"}}}); perr != nil {
		t.Fatalf("a change to the object this apply created: %v", perr)
	}
	if got := strings.Join(api.got, "\n"); !strings.Contains(got, "POST /storage/local/upload content=iso checksum="+strings.Repeat("a", 64)+" file:filename=seed-web-1234.iso") ||
		strings.Contains(got, "no length") || api.files["seed-web-1234.iso"] != "seed with token-web" {
		t.Fatalf("API received:\n%s\nfiles %v", got, api.files)
	}
}

// A change whose artifacts are not well formed, or whose requests name others, is not a permit.
func TestArtifactsAreChecked(t *testing.T) {
	good := artifactPermit(t)
	m := protocol.Manifest{Schema: protocol.Schema, Provider: "p", Version: "1", API: "test", Kinds: []protocol.Kind{{Kind: "VM",
		Operations: []protocol.Operation{{Op: "create", Support: protocol.Supported, Calls: good.Calls}}}}}
	base := protocol.Change{Resource: "VM web", Kind: "VM", Op: "create", IDs: good.IDs, Artifacts: good.Artifacts, Requests: good.Requests}
	for name, mutate := range map[string]func(c *protocol.Change){
		"http download": func(c *protocol.Change) {
			c.Artifacts = append(c.Artifacts, protocol.Artifact{ID: "img", Kind: protocol.ArtifactDownload, URL: "http://x/i", SHA256: strings.Repeat("a", 64), Name: "i.img"})
		},
		"download without sha256": func(c *protocol.Change) {
			c.Artifacts = append(c.Artifacts, protocol.Artifact{ID: "img", Kind: protocol.ArtifactDownload, URL: "https://x/i", Name: "i.img"})
		},
		"a seed with a name": func(c *protocol.Change) {
			c.Artifacts = []protocol.Artifact{{ID: "seed", Kind: protocol.ArtifactSeed, Node: "web", NodeID: 7, Name: "x.iso", Network: &protocol.Network{Mode: "dhcp"}}}
		},
		"unknown artifact referenced": func(c *protocol.Change) {
			c.Requests = append([]protocol.Approved{}, c.Requests...)
			c.Requests[1] = protocol.Approved{Method: "POST", Path: "/vms", Params: map[string][]string{"vm": {"7"}, "cdrom": {"{artifact:other.name}"}}}
		},
		"upload on a call without a file": func(c *protocol.Change) {
			c.Requests = append([]protocol.Approved{}, c.Requests...)
			c.Requests[1].Upload = "seed"
		},
		"an upload call without an upload": func(c *protocol.Change) {
			c.Requests = append([]protocol.Approved{}, c.Requests...)
			c.Requests[0].Upload = ""
		},
	} {
		c := base
		mutate(&c)
		if _, err := protocol.NewPermit(m, c, "op", "", "", "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := protocol.NewPermit(m, base, "op", "", "", "x"); err != nil {
		t.Fatal(err)
	}
}
