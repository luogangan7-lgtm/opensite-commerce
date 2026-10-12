// Catalog validation tests exercise identifier, cursor and optimistic-version
// trust boundaries; stdlib testing calls package helpers without database I/O.
package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeCode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		invalid  bool
	}{
		{" \t ab-c.1_ \r\n", "AB-C.1_", false}, {"", "", true},
		{"\u00a0sku", "", true}, {"商品", "", true}, {"a b", "", true}, {"a/b", "", true},
		{strings.Repeat("a", 128), strings.Repeat("A", 128), false}, {strings.Repeat("a", 129), "", true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeCode(tc.in)
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("got %q %v", got, err)
			}
		})
	}
}

func TestPageLimit(t *testing.T) {
	if n, err := pageLimit(nil); n != 20 || err != nil {
		t.Fatalf("default %d %v", n, err)
	}
	for _, tc := range []struct {
		in   int
		want int32
		bad  bool
	}{{0, 0, true}, {-1, 0, true}, {1, 1, false}, {100, 100, false}, {101, 0, true}, {int(^uint(0) >> 1), 0, true}} {
		n, err := pageLimit(&tc.in)
		if (err != nil) != tc.bad || n != tc.want {
			t.Fatalf("limit %d = %d %v", tc.in, n, err)
		}
	}
}
func TestVersionHeader(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{`"1"`, 1, false}, {`"9223372036854775807"`, 9223372036854775807, false},
		{"", 0, true}, {"1", 0, true}, {`W/"1"`, 0, true}, {`"0"`, 0, true}, {`"-1"`, 0, true}, {`"01"`, 0, true}, {`"9223372036854775808"`, 0, true}, {`"1", "2"`, 0, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseVersion(tc.in)
			if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
				t.Fatalf("got %d %v", got, err)
			}
		})
	}
}
func TestCursorValidation(t *testing.T) {
	tm := time.Date(2026, 1, 1, 1, 2, 3, 456000000, time.UTC)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	c := encodeCursor(tm, id)
	got, gotID, has, err := parseCursor(&c)
	if err != nil || !has || !got.Equal(tm) || gotID != id {
		t.Fatalf("roundtrip: %v %v %v %v", got, gotID, has, err)
	}
	for _, s := range []string{"", "!", "e30", "eyJ0IjoiYmFkIiwiaWQiOiJub3QtaWQifQ"} {
		if _, _, _, err := parseCursor(&s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
