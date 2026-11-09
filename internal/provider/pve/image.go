package pve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Image is a pinned cloud image a VM can be created from. Nothing else is accepted: an image name
// in a declaration must be in the catalog, and the file must match its SHA-256 exactly.
type Image struct {
	URL    string
	SHA256 string // hex
	Size   int64  // bytes of the file as published
	// MinDiskGiB is the smallest disk a VM made from this image can have: the image's virtual size,
	// rounded up. Proxmox can grow a disk but never shrinks it.
	MinDiskGiB int64
}

// Catalog is the built-in set of images. Changing an entry is a reviewed code change.
var Catalog = map[string]Image{
	// Ubuntu 24.04 LTS server cloud image, release 20261017 (qcow2, virtual size 3.5 GiB).
	// From https://cloud-images.ubuntu.com/releases/noble/release-20261017/SHA256SUMS
	"ubuntu-24.04": {
		URL:        "https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64.img",
		SHA256:     "04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94",
		Size:       621284352,
		MinDiskGiB: 4,
	},
}

// VolumeName is the file name of an image on the Proxmox image storage. It contains the checksum,
// so a different file can never be mistaken for it, and ends in .qcow2 as Proxmox requires for
// importable images.
func VolumeName(name string, img Image) string {
	return fmt.Sprintf("mamela-%s-%s.qcow2", name, img.SHA256[:16])
}

// ImageCache keeps verified images on the machine that runs Mamela. Files are named by checksum
// and verified again every time they are opened.
type ImageCache struct {
	Dir  string
	HTTP *http.Client // nil: a client with a 30 minute timeout
}

// Open returns the image file, downloading it first if it is not cached. The caller closes it.
func (c *ImageCache) Open(ctx context.Context, img Image) (ImageFile, error) {
	path := filepath.Join(c.Dir, img.SHA256+".qcow2")
	if f, err := c.verified(path, img); err == nil {
		return f, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := c.download(ctx, img, path); err != nil {
		return nil, err
	}
	return c.verified(path, img)
}

// verified opens path and checks its size, checksum and qcow2 header, leaving it at offset 0.
func (c *ImageCache) verified(path string, img Image) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := check(f, img); err != nil {
		f.Close()
		return nil, fmt.Errorf("cached image %s: %w (delete it to download again)", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

func check(r io.Reader, img Image) error {
	h := sha256.New()
	head := make([]byte, 4)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	h.Write(head[:n])
	rest, err := io.Copy(h, r)
	if err != nil {
		return err
	}
	if size := int64(n) + rest; size != img.Size {
		return fmt.Errorf("size is %d bytes, want %d", size, img.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != img.SHA256 {
		return fmt.Errorf("sha256 is %s, want %s", got, img.SHA256)
	}
	if !bytes.Equal(head[:n], qcow2Magic) {
		return errors.New("not a qcow2 image")
	}
	return nil
}

func (c *ImageCache) download(ctx context.Context, img Image, path string) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", img.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", img.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(c.Dir, "download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, io.LimitReader(resp.Body, img.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", img.URL, err)
	}
	f, err := os.Open(tmp.Name())
	if err != nil {
		return err
	}
	err = check(f, img)
	f.Close()
	if err != nil {
		return fmt.Errorf("download %s: %w", img.URL, err)
	}
	return os.Rename(tmp.Name(), path)
}
