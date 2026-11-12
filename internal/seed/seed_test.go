package seed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
)

// readISO is a minimal ISO 9660 reader: the volume label and the root directory's files, with
// names as Linux shows them (lowercase, without ".;1").
func readISO(t *testing.T, img []byte) (string, map[string][]byte) {
	t.Helper()
	if len(img)%sector != 0 {
		t.Fatalf("size %d is not a multiple of %d", len(img), sector)
	}
	pvd := img[16*sector:]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatal("no primary volume descriptor")
	}
	if got := binary.LittleEndian.Uint32(pvd[80:84]); int(got)*sector != len(img) {
		t.Fatalf("volume size %d sectors, image %d bytes", got, len(img))
	}
	if binary.LittleEndian.Uint32(pvd[80:84]) != binary.BigEndian.Uint32(pvd[84:88]) {
		t.Fatal("both-endian volume size disagrees")
	}
	if term := img[17*sector:]; term[0] != 255 || string(term[1:6]) != "CD001" {
		t.Fatal("no terminator")
	}
	label := strings.TrimRight(string(pvd[40:72]), " ")
	root := pvd[156:190]
	at := binary.LittleEndian.Uint32(root[2:6])
	size := binary.LittleEndian.Uint32(root[10:14])
	dir := img[int(at)*sector : int(at)*sector+int(size)]
	files := map[string][]byte{}
	for off := 0; off < len(dir) && dir[off] != 0; off += int(dir[off]) {
		r := dir[off : off+int(dir[off])]
		id := string(r[33 : 33+int(r[32])])
		if r[25]&2 != 0 {
			continue // . and ..
		}
		name := strings.ToLower(strings.TrimSuffix(id, ".;1"))
		ext := binary.LittleEndian.Uint32(r[2:6])
		n := binary.LittleEndian.Uint32(r[10:14])
		files[name] = img[int(ext)*sector : int(ext)*sector+int(n)]
	}
	return label, files
}

func params() Params {
	return Params{Node: "web", VMID: 7001, User: "mamela", SSHPublicKeys: []string{"ssh-ed25519 AAAA alice@laptop"},
		EnrollToken: "tok-123", MameletBinary: bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 1500), Network: graph.Network{Mode: graph.NetworkDHCP},
		Mamelet: mamelet.Config{Node: "web", VMID: 7001, Server: "https://127.0.0.1:8443", CA: "-----BEGIN CERTIFICATE-----\n",
			RevisionKey: make([]byte, ed25519.PublicKeySize), StateDir: "/var/lib/mamelet", AllowedOrigins: []string{"Ubuntu:"}}}
}

func TestSeedImage(t *testing.T) {
	files, err := Files(params())
	if err != nil {
		t.Fatal(err)
	}
	img, err := ISO("CIDATA", files)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ISO("CIDATA", files)
	if !bytes.Equal(img, again) {
		t.Fatal("the image is not reproducible")
	}
	label, got := readISO(t, img)
	if label != "CIDATA" || len(got) != 4 {
		t.Fatalf("label %q files %v", label, len(got))
	}
	if !bytes.Equal(got["mamelet"], params().MameletBinary) {
		t.Fatal("mamelet binary differs")
	}
	if string(got["meta-data"]) != "instance-id: mamela-web-7001\nlocal-hostname: web\n" {
		t.Fatalf("meta-data %q", got["meta-data"])
	}
	ud := got["user-data"]
	if !bytes.HasPrefix(ud, []byte("#cloud-config\n")) {
		t.Fatal("user-data lacks #cloud-config")
	}
	var cc map[string]any
	if err := json.Unmarshal(ud[len("#cloud-config\n"):], &cc); err != nil {
		t.Fatal(err)
	}
	users := cc["users"].([]any)[0].(map[string]any)
	if users["name"] != "mamela" || users["lock_passwd"] != true || cc["ssh_pwauth"] != false {
		t.Fatalf("users %v", users)
	}
	wf := cc["write_files"].([]any)
	var cfg mamelet.Config
	if err := json.Unmarshal([]byte(wf[0].(map[string]any)["content"].(string)), &cfg); err != nil || cfg.Node != "web" {
		t.Fatalf("mamelet config: %v %+v", err, cfg)
	}
	if tok := wf[1].(map[string]any); tok["content"] != "tok-123\n" || tok["permissions"] != "0600" {
		t.Fatalf("token file %v", tok)
	}
	if !strings.Contains(string(ud), "sha256sum -c") {
		t.Fatal("the binary is not verified before it is installed")
	}
	if string(got["network-config"]) != `{"ethernets":{"primary":{"dhcp4":true,"match":{"name":"e*"}}},"version":2}` {
		t.Fatalf("network-config %s", got["network-config"])
	}
}

// A static network is written as netplan: the address, a default route and the name servers.
func TestSeedStaticNetwork(t *testing.T) {
	p := params()
	p.Network = graph.Network{Mode: graph.NetworkStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1", "198.51.100.1"}}
	files, err := Files(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ethernets":{"primary":{"addresses":["192.0.2.10/24"],"dhcp4":false,"match":{"name":"e*"},` +
		`"nameservers":{"addresses":["192.0.2.1","198.51.100.1"]},"routes":[{"to":"default","via":"192.0.2.1"}]}},"version":2}`
	for _, f := range files {
		if f.Name == "network-config" && string(f.Data) != want {
			t.Fatalf("network-config\n%s\nwant\n%s", f.Data, want)
		}
	}
	p.Network = graph.Network{Mode: graph.NetworkStatic, Address: "192.0.2.10/24"}
	if _, err := Files(p); err == nil {
		t.Fatal("made a static network without a gateway and name servers")
	}
	p.Network = graph.Network{}
	if _, err := Files(p); err == nil {
		t.Fatal("made a seed without a network mode")
	}
}

func TestSeedRefusesBadInput(t *testing.T) {
	p := params()
	p.SSHPublicKeys = []string{"ssh-ed25519 AAAA\nruncmd: [evil]"}
	if _, err := Files(p); err == nil {
		t.Fatal("accepted a multi-line key")
	}
	p = params()
	p.Node = "Web Server"
	if _, err := Files(p); err == nil {
		t.Fatal("accepted a bad node name")
	}
	if _, err := ISO("CIDATA", []File{{Name: "../etc/passwd"}}); err == nil {
		t.Fatal("accepted a path as a file name")
	}
	if _, err := ISO("cidata", nil); err == nil {
		t.Fatal("accepted a lowercase label (ISO 9660 d-characters are uppercase)")
	}
}

type memStore struct {
	seeds    map[string]string
	n        int
	reusable string
}

func (m *memStore) IssueEnrollToken(_ context.Context, node string, vmid int, seed string, ttl time.Duration) (string, error) {
	m.n++
	return "token-" + string(rune('a'+m.n)), nil
}
func (m *memStore) SetEnrollSeed(_ context.Context, token, seed string) error {
	m.seeds[token] = seed
	return nil
}
func (m *memStore) ReusableSeed(context.Context, string, int, time.Duration) (string, bool, error) {
	return m.reusable, m.reusable != "", nil
}

func TestMakerNamesSeedsByContent(t *testing.T) {
	st := &memStore{seeds: map[string]string{}}
	m := &Maker{Store: st, Server: "https://127.0.0.1:8443", CA: []byte("ca"), RevisionKey: make([]byte, 32), User: "mamela",
		AllowedOrigins: []string{"Ubuntu:"}, MameletBinary: []byte("bin")}
	dhcp := graph.Network{Mode: graph.NetworkDHCP}
	a, imgA, err := m.New(context.Background(), "web", 7001, dhcp)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := m.New(context.Background(), "web", 7001, dhcp)
	if a == b || !strings.HasPrefix(a, "mamela-seed-web-7001-") || !strings.HasSuffix(a, ".iso") {
		t.Fatalf("names %s %s (each seed has its own token, so its own name)", a, b)
	}
	if st.seeds["token-b"] != a || len(imgA) == 0 {
		t.Fatalf("seed not recorded with its token: %v", st.seeds)
	}
	// A seed made for one network is never reused for another.
	st.reusable = a
	if f, ok, _ := m.Reusable(context.Background(), "web", 7001, dhcp); !ok || f != a {
		t.Fatalf("the seed for the same network is not reused: %q %v", f, ok)
	}
	static := graph.Network{Mode: graph.NetworkStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	if f, ok, _ := m.Reusable(context.Background(), "web", 7001, static); ok {
		t.Fatalf("a DHCP seed %q is reused for a static network", f)
	}
}

// A cloud machine's seed is user-data alone: it fetches mamelet over https from the Control Plane,
// verified with Mamela's CA, and installs it only if its sha256 is the one Mamela put there; it
// fits a cloud's limit and configures no network.
func TestUserDataSeed(t *testing.T) {
	p := params()
	sha := strings.Repeat("ab", 32)
	ud, err := UserData(p, "https://cp.example:8443/v1/mamelet/"+sha, sha)
	if err != nil {
		t.Fatal(err)
	}
	text := string(ud)
	for _, want := range []string{"#cloud-config\n", "--cacert /etc/mamelet/ca.pem", "--proto =https", sha + "  $f\\\" | sha256sum -c -", "tok-123", "/etc/mamelet/ca.pem"} {
		if !strings.Contains(text, want) {
			t.Errorf("user-data lacks %q", want)
		}
	}
	if len(ud) > MaxUserData || strings.Contains(text, "ethernets") || strings.Contains(text, "qemu-guest-agent") {
		t.Fatalf("user-data (%d bytes):\n%s", len(ud), text)
	}
	if _, err := UserData(p, "http://cp.example/v1/mamelet/x", sha); err == nil {
		t.Error("fetched mamelet over http")
	}
	if _, err := UserData(p, "https://cp.example/v1/mamelet/x'; rm -rf /", sha); err == nil {
		t.Error("a URL that breaks out of the command")
	}
	p.Network = graph.Network{Mode: graph.NetworkStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	if _, err := UserData(p, "https://cp.example/v1/mamelet/"+sha, sha); err == nil {
		t.Error("a static network in user-data")
	}
}
