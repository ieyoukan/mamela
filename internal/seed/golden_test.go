package seed

// The golden tests fix, byte for byte, what a seed holds: the user-data and network-config of a
// Proxmox seed image, the cloud user-data, the plain seed, and the seed image itself (its sha256).
// The enrollment script for existing machines (enroll.go) shares the seed's files; sharing them must
// not change one byte of a seed. Regenerate them only for a change that means to change a seed:
//
//	go test ./internal/seed -run Golden -update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with -update to write it)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestGoldenSeeds(t *testing.T) {
	p := params()
	p.Network = graph.Network{Mode: graph.NetworkStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	files, err := Files(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name != "mamelet" {
			golden(t, "iso-"+f.Name, f.Data)
		}
	}
	img, err := ISO("CIDATA", files)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(img)
	golden(t, "iso.sha256", []byte(hex.EncodeToString(sum[:])+"\n"))

	sha := strings.Repeat("ab", 32)
	ud, err := UserData(params(), "https://cp.example:8443/v1/mamelet/"+sha, sha)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "cloud-user-data", ud)

	plain, err := PlainFiles(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plain {
		golden(t, "plain-"+f.Name, f.Data)
	}
	plainUD, err := PlainUserData(params())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "plain-cloud-user-data", plainUD)

	// The maker's seeds: the configuration it writes for mamelet (its paths, interval, origins).
	m := &Maker{Store: &memStore{seeds: map[string]string{}}, Server: "https://127.0.0.1:8443", CA: []byte("-----BEGIN CERTIFICATE-----\n"),
		RevisionKey: make([]byte, 32), User: "mamela", SSHPublicKeys: p.SSHPublicKeys, AllowedOrigins: []string{"Ubuntu:"}, MameletBinary: p.MameletBinary}
	_, made, err := m.NewUserData(context.Background(), "web", 7001, graph.Network{Mode: graph.NetworkDHCP}, "https://cp.example:8443/v1/mamelet/"+sha)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "maker-cloud-user-data", made)
	name, img, err := m.New(context.Background(), "web", 7001, p.Network)
	if err != nil {
		t.Fatal(err)
	}
	_, inImage := readISO(t, img)
	golden(t, "maker-iso-user-data", inImage["user-data"])
	golden(t, "maker-iso.name", []byte(name+"\n"))
}
