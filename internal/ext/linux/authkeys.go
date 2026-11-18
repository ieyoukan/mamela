package linux

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/safetext"
)

// The lines of a user's ~/.ssh/authorized_keys (NodeUser). A declaration writes each line as
// OpenSSH writes a public key, "<type> <base64> [comment]", and the graph and mamelet read it with
// the same function (ParseAuthorizedKey). Options before the key (command=, from=, restrict, ...)
// are not taken in this release: they change what a key may do, and the plan would have to show
// what they mean.

// KeyTypes are the key types a declaration may write, as OpenSSH names them.
var KeyTypes = []string{
	"ssh-ed25519",
	"sk-ssh-ed25519@openssh.com",
	"ecdsa-sha2-nistp256",
	"ecdsa-sha2-nistp384",
	"ecdsa-sha2-nistp521",
	"sk-ecdsa-sha2-nistp256@openssh.com",
	"ssh-rsa",
}

// MaxKeyLine bounds one line of authorized_keys, in bytes (a 16384-bit RSA key is about 2.8 KB).
const MaxKeyLine = 8 << 10

// MaxKeys bounds the keys of one user.
const MaxKeys = 64

// AuthorizedKey is a public key of authorized_keys, as the plan shows it.
type AuthorizedKey struct {
	Type        string
	Fingerprint string // "SHA256:<base64 without padding>", as ssh-keygen -l shows it
	Comment     string
}

// String is the key for a person: its type, fingerprint and comment.
func (k AuthorizedKey) String() string {
	if k.Comment == "" {
		return k.Type + " " + k.Fingerprint
	}
	return k.Type + " " + k.Fingerprint + " " + k.Comment
}

// keyOptions are the words an option prefix of an authorized_keys line starts with (OpenSSH's
// sshd(8)), so that a line with options is told apart from a key of an unknown type.
var keyOptions = []string{"agent-forwarding", "cert-authority", "command=", "environment=", "expiry-time=", "from=",
	"no-agent-forwarding", "no-port-forwarding", "no-pty", "no-user-rc", "no-X11-forwarding", "no-touch-required",
	"permitlisten=", "permitopen=", "port-forwarding", "principals=", "pty", "restrict", "tunnel=", "user-rc",
	"verify-required", "X11-forwarding"}

// ParseAuthorizedKey reads one line of authorized_keys as a declaration writes it: a known key type,
// one space, the key in base64 (which decodes, and whose embedded type is the line's), and an
// optional comment after one space; no option before the key, no control character, nothing before
// or after.
func ParseAuthorizedKey(line string) (AuthorizedKey, error) {
	switch {
	case line == "":
		return AuthorizedKey{}, fmt.Errorf("the line is empty")
	case len(line) > MaxKeyLine:
		return AuthorizedKey{}, fmt.Errorf("the line is %d bytes; at most %d", len(line), MaxKeyLine)
	}
	if f, bad := safetext.Find(line, false); bad {
		return AuthorizedKey{}, fmt.Errorf("the line has %s", f)
	}
	if strings.TrimSpace(line) != line {
		return AuthorizedKey{}, fmt.Errorf("the line starts or ends with a space")
	}
	typ, rest, _ := strings.Cut(line, " ")
	if !slices.Contains(KeyTypes, typ) {
		for _, o := range keyOptions {
			if strings.HasPrefix(typ, o) && (strings.HasSuffix(o, "=") || len(typ) == len(o) || typ[len(o)] == ',') {
				return AuthorizedKey{}, fmt.Errorf("the line has options before the key (%s); Mamela does not write options in this release", o)
			}
		}
		return AuthorizedKey{}, fmt.Errorf("%q is not a key type Mamela takes (%s)", short(typ), strings.Join(KeyTypes, ", "))
	}
	b64, comment, hasComment := strings.Cut(rest, " ")
	switch {
	case b64 == "":
		return AuthorizedKey{}, fmt.Errorf("the line has no key after %s (or two spaces)", typ)
	case hasComment && (comment == "" || strings.HasPrefix(comment, " ")):
		return AuthorizedKey{}, fmt.Errorf("the comment is empty or after more than one space")
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return AuthorizedKey{}, fmt.Errorf("the key is not base64: %v", err)
	}
	if embedded, ok := blobType(blob); !ok || embedded != typ {
		return AuthorizedKey{}, fmt.Errorf("the key does not decode as a %s key", typ)
	}
	return AuthorizedKey{Type: typ, Fingerprint: fingerprint(blob), Comment: comment}, nil
}

// blobType is the key type written at the start of a public key (the SSH wire form: a uint32
// length, then the type), and whether there is anything after it.
func blobType(blob []byte) (string, bool) {
	if len(blob) < 4 {
		return "", false
	}
	n := binary.BigEndian.Uint32(blob)
	if n == 0 || uint64(n)+4 >= uint64(len(blob)) {
		return "", false
	}
	return string(blob[4 : 4+n]), true
}

// fingerprint is a public key's fingerprint as OpenSSH shows it.
func fingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// KeysContent is the content of authorized_keys with exactly these lines, in this order.
func KeysContent(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// FoundKey is a key mamelet found in a user's authorized_keys: its type and fingerprint (never its
// comment), and whether the line had options before it.
type FoundKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Options     bool   `json:"options,omitzero"`
}

// scanAuthorizedKeys reads the keys of an authorized_keys file as sshd would find them, options
// included, to say which the plan removes: the keys (in the file's order), and how many other
// lines are neither a key, a comment nor empty (a key of a type Mamela does not know, a
// certificate, a line it cannot read).
func scanAuthorizedKeys(content string) (keys []FoundKey, other int) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := splitOutsideQuotes(line)
		found := false
		for i, f := range fields {
			if !slices.Contains(KeyTypes, f) || i+1 >= len(fields) {
				continue
			}
			blob, err := base64.StdEncoding.DecodeString(fields[i+1])
			if t, ok := blobType(blob); err != nil || !ok || t != f {
				break
			}
			keys = append(keys, FoundKey{Type: f, Fingerprint: fingerprint(blob), Options: i > 0})
			found = true
			break
		}
		if !found {
			other++
		}
	}
	return keys, other
}

// splitOutsideQuotes splits a line at spaces and tabs that are not inside double quotes (an option
// such as command="a b" is one field).
func splitOutsideQuotes(line string) []string {
	var out []string
	var cur strings.Builder
	quoted, escaped := false, false
	for _, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quoted:
			escaped = true
		case r == '"':
			quoted = !quoted
		case (r == ' ' || r == '\t') && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// short is a value cut for a message.
func short(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
