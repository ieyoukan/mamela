package linux

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// code is "MAM-PLAN" and 140 of "MAM-PLAN-0140".
func splitCode(t *testing.T, c string) (string, int) {
	t.Helper()
	i := strings.LastIndex(c, "-")
	n, err := strconv.Atoi(c[i+1:])
	if err != nil {
		t.Fatalf("%q is not a code", c)
	}
	return c[:i], n
}

func reservedFor(prefix string, n int) (reservation, bool) {
	for _, r := range reserved {
		if r.Prefix == prefix && r.From <= n && n <= r.To {
			return r, true
		}
	}
	return reservation{}, false
}

// Every code the extension registers is in its kind's ranges, and no two ranges overlap.
func TestEveryCodeIsInItsKindsRange(t *testing.T) {
	for i, a := range reserved {
		for _, b := range reserved[i+1:] {
			if a.Prefix == b.Prefix && a.From <= b.To && b.From <= a.To {
				t.Errorf("%+v and %+v overlap", a, b)
			}
		}
	}
	for _, k := range kinds {
		var codes []string
		codes = append(codes, k.codes...)
		for _, w := range k.warningCodes {
			codes = append(codes, string(w))
		}
		for _, o := range k.ops {
			codes = append(codes, string(o.decision.Code))
		}
		if len(codes) == 0 {
			t.Errorf("%s registers no code", k.def.Kind)
		}
		for _, c := range codes {
			prefix, n := splitCode(t, c)
			if r, ok := reservedFor(prefix, n); !ok || r.Kind != k.def.Kind {
				t.Errorf("%s: %s is not in its ranges (%+v)", k.def.Kind, c, r)
			}
		}
	}
}

// docs/reason-codes.md has every reserved code, as a row of its own or in a reserved range, and a
// reserved range there names the kind it is reserved for here (or none, for the kinds that follow).
func TestTheReservedCodesAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/reason-codes.md")
	if err != nil {
		t.Skip("no docs/reason-codes.md: ", err)
	}
	row := regexp.MustCompile(`^\| (MAM-[A-Z]+)-(\d{4})(?:〜(\d{4}))? \|(.*)$`)
	type docRow struct {
		prefix   string
		from, to int
		text     string
	}
	var rows []docRow
	for _, line := range strings.Split(string(doc), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		from, _ := strconv.Atoi(m[2])
		to := from
		if m[3] != "" {
			to, _ = strconv.Atoi(m[3])
		}
		rows = append(rows, docRow{m[1], from, to, m[4]})
	}
	kindsNamed := func(text string) []graph.Kind {
		var out []graph.Kind
		for _, k := range kinds {
			if strings.Contains(text, string(k.def.Kind)) {
				out = append(out, k.def.Kind)
			}
		}
		return out
	}
	for _, r := range reserved {
		for n := r.From; n <= r.To; n++ {
			code := fmt.Sprintf("%s-%04d", r.Prefix, n)
			found := false
			for _, d := range rows {
				if d.prefix != r.Prefix || n < d.from || n > d.to {
					continue
				}
				found = true
				if !strings.Contains(d.text, "予約") {
					continue // a code in use: its row says what it is
				}
				named := kindsNamed(d.text)
				if r.Kind == "" && len(named) > 0 || r.Kind != "" && (len(named) != 1 || named[0] != r.Kind) {
					t.Errorf("%s is reserved for %q in codes.go, and for %v in docs/reason-codes.md", code, r.Kind, named)
				}
			}
			if !found {
				t.Errorf("%s (%q) is not in docs/reason-codes.md", code, r.Kind)
			}
		}
	}
	// And the other way: what the docs reserve for linux is reserved here, for the same kind.
	for _, d := range rows {
		if !strings.Contains(d.text, "予約（linux") {
			continue
		}
		named := kindsNamed(d.text)
		for n := d.from; n <= d.to; n++ {
			r, ok := reservedFor(d.prefix, n)
			if !ok || len(named) == 1 && r.Kind != named[0] || len(named) == 0 && r.Kind != "" {
				t.Errorf("docs/reason-codes.md reserves %s-%04d for linux (%v); codes.go has %+v", d.prefix, n, named, r)
			}
		}
	}
}
