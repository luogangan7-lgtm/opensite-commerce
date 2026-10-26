// AVIF association tests use the committed encoded image as their witness.
// Dependencies: bounded BMFF parser; callers: media content gate.
// Invariant: only properties associated with the primary item describe it.
package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func TestAVIFPrimaryPropertiesMustBeAssociated(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	ipma := bytes.Index(fixture, []byte("ipma")) + 4
	if ipma < 4 {
		t.Fatal("fixture has no ipma")
	}
	for _, tc := range []struct {
		name   string
		offset int
		value  byte
	}{
		{"different item", 9, 2},
		{"dimensions unassociated", 11, 2},
		{"AV1 configuration unassociated", 13, 0},
		{"out of range property", 11, 127},
		{"unsupported flags", 3, 2},
		{"unsupported version", 0, 2},
		{"no item association", 7, 0},
		{"duplicate dimension association", 13, 129},
		{"essential zero association", 12, 0x80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := bytes.Clone(fixture)
			input[ipma+tc.offset] = tc.value
			if _, _, _, err := inspect(input, "image/avif"); err == nil {
				t.Fatal("primary image accepted with invalid property association")
			}
		})
	}
}

// Rebuild iprp while moving the committed primary extent with its mdat.
func avifWithProperties(t *testing.T, properties, association []byte) []byte {
	t.Helper()
	return avifWithIPRP(t, func(makeBox func(string, []byte) []byte) []byte {
		return append(makeBox("ipco", properties), makeBox("ipma", association)...)
	})
}

// avifWithIPRP lets a test choose the exact iprp children (container order/count).
func avifWithIPRP(t *testing.T, children func(func(string, []byte) []byte) []byte) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	makeBox := func(kind string, data []byte) []byte {
		out := make([]byte, 8+len(data))
		binary.BigEndian.PutUint32(out, uint32(len(out)))
		copy(out[4:8], kind)
		copy(out[8:], data)
		return out
	}
	pos := bytes.Index(b, []byte("iprp")) - 4
	oldSize := int(binary.BigEndian.Uint32(b[pos:]))
	next := makeBox("iprp", children(makeBox))
	delta := len(next) - oldSize
	out := append(bytes.Clone(b[:pos]), next...)
	out = append(out, b[pos+oldSize:]...)
	meta := bytes.Index(out, []byte("meta")) - 4
	binary.BigEndian.PutUint32(out[meta:], uint32(int(binary.BigEndian.Uint32(out[meta:]))+delta))
	iloc := bytes.Index(out, []byte("iloc")) - 4
	offset := iloc + int(binary.BigEndian.Uint32(out[iloc:])) - 8
	binary.BigEndian.PutUint32(out[offset:], uint32(int(binary.BigEndian.Uint32(out[offset:]))+delta))
	return out
}

func TestAVIFPrimaryAssociationVersions(t *testing.T) {
	b, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(b, []byte("ipco")) + 4
	size := int(binary.BigEndian.Uint32(b[start-8:])) - 8
	props := bytes.Clone(b[start : start+size])
	// An unassociated second ispe is legal and must not replace primary dimensions.
	props = append(props, props[:20]...)
	// ISO-BMFF permits 15-bit indices only when ipco has more than 127 properties.
	wideProps := bytes.Clone(props)
	for range 128 {
		wideProps = append(wideProps, 0, 0, 0, 8, 'f', 'r', 'e', 'e')
	}
	for _, tc := range []struct {
		name         string
		associations []byte
		wide         bool
	}{
		{"narrow version0", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 4, 1, 2, 0x83, 4}, false},
		{"wide version0", []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 4, 0, 1, 0, 2, 0x80, 3, 0, 4}, true},
		{"wide version1", []byte{1, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 4, 0, 1, 0, 2, 0x80, 3, 0, 4}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := props
			if tc.wide {
				table = wideProps
			}
			_, w, h, err := inspect(avifWithProperties(t, table, tc.associations), "image/avif")
			if err != nil || w != 2 || h != 3 {
				t.Fatalf("primary dimensions=%dx%d err=%v", w, h, err)
			}
		})
	}
}

func TestAVIFZeroAssociationEssentialBit(t *testing.T) {
	b, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(b, []byte("ipco")) + 4
	size := int(binary.BigEndian.Uint32(b[start-8:])) - 8
	props := bytes.Clone(b[start : start+size])
	for range 128 { // wide indices require more than 127 properties
		props = append(props, 0, 0, 0, 8, 'f', 'r', 'e', 'e')
	}
	for _, tc := range []struct {
		name         string
		associations []byte
		invalid      bool
	}{
		{"narrow zero", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 3, 1, 0, 0x83}, false},
		{"narrow essential zero", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 3, 1, 0x80, 0x83}, true},
		{"wide zero", []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 3, 0, 1, 0, 0, 0x80, 3}, false},
		{"wide essential zero", []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 3, 0, 1, 0x80, 0, 0x80, 3}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, h, err := inspect(avifWithProperties(t, props, tc.associations), "image/avif")
			if tc.invalid {
				if err == nil {
					t.Fatal("essential association cannot have property index zero")
				}
			} else if err != nil || w != 2 || h != 3 {
				t.Fatalf("valid zero association rejected: dimensions=%dx%d err=%v", w, h, err)
			}
		})
	}
}

// Review hardening: ordering, wide-index preconditions and container uniqueness.
func TestAVIFIpmaOrderingWidthAndContainerUniqueness(t *testing.T) {
	b, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(b, []byte("ipco")) + 4
	size := int(binary.BigEndian.Uint32(b[start-8:])) - 8
	props := bytes.Clone(b[start : start+size])
	primary := []byte{1, 4, 1, 2, 0x83, 4}
	for _, tc := range []struct {
		name         string
		associations []byte
		invalid      bool
	}{
		{"ascending", append([]byte{0, 0, 0, 0, 0, 0, 0, 2, 0, 1}, append(primary[1:], 0, 2, 1, 1)...), false},
		{"descending", append([]byte{0, 0, 0, 0, 0, 0, 0, 2, 0, 2, 1, 1, 0, 1}, primary[1:]...), true},
		{"wide flags for small table", []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 4, 0, 1, 0, 2, 0x80, 3, 0, 4}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, h, err := inspect(avifWithProperties(t, props, tc.associations), "image/avif")
			if tc.invalid != (err != nil) || (!tc.invalid && (w != 2 || h != 3)) {
				t.Fatalf("invalid=%v dimensions=%dx%d err=%v", tc.invalid, w, h, err)
			}
		})
	}
	t.Run("ipma before ipco", func(t *testing.T) {
		input := avifWithIPRP(t, func(makeBox func(string, []byte) []byte) []byte {
			out := makeBox("ipma", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 4, 1, 2, 0x83, 4})
			return append(out, makeBox("ipco", props)...)
		})
		if _, _, _, err := inspect(input, "image/avif"); err == nil {
			t.Fatal("association box accepted before the property container")
		}
	})
	t.Run("empty first ipco then second ipco", func(t *testing.T) {
		input := avifWithIPRP(t, func(makeBox func(string, []byte) []byte) []byte {
			out := makeBox("ipco", nil)
			out = append(out, makeBox("ipco", props)...)
			return append(out, makeBox("ipma", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 4, 1, 2, 0x83, 4})...)
		})
		if _, _, _, err := inspect(input, "image/avif"); err == nil {
			t.Fatal("second property container accepted after an empty first one")
		}
	})
}

// A pitm that names item 0 is still a declaration; a second pitm must be rejected.
func TestAVIFSecondPrimaryItemBoxAfterZeroID(t *testing.T) {
	b, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	pos := bytes.Index(b, []byte("pitm")) - 4
	size := int(binary.BigEndian.Uint32(b[pos:]))
	zero := bytes.Clone(b[pos : pos+size])
	binary.BigEndian.PutUint16(zero[len(zero)-2:], 0)
	if size != 14 {
		t.Fatal("fixture pitm layout changed")
	}
	out := append(bytes.Clone(b[:pos]), zero...)
	out = append(out, b[pos:pos+size]...)
	out = append(out, b[pos+size:]...)
	meta := bytes.Index(out, []byte("meta")) - 4
	binary.BigEndian.PutUint32(out[meta:], uint32(int(binary.BigEndian.Uint32(out[meta:]))+size))
	iloc := bytes.Index(out, []byte("iloc")) - 4
	offset := iloc + int(binary.BigEndian.Uint32(out[iloc:])) - 8
	binary.BigEndian.PutUint32(out[offset:], uint32(int(binary.BigEndian.Uint32(out[offset:]))+size))
	if _, _, _, err := inspect(out, "image/avif"); err == nil {
		t.Fatal("second pitm accepted after a zero-ID first pitm")
	}
}

func maxDimensionAVIF(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tiny.avif")
	if err != nil {
		t.Fatal(err)
	}
	pos := bytes.Index(b, []byte("ispe")) + 8
	if pos < 8 || pos+8 > len(b) {
		t.Fatal("fixture missing primary dimensions")
	}
	binary.BigEndian.PutUint32(b[pos:pos+4], ^uint32(0))
	binary.BigEndian.PutUint32(b[pos+4:pos+8], ^uint32(0))
	return b
}

func TestAVIFUint32DimensionsDoNotOverflow(t *testing.T) {
	if _, _, _, err := inspect(maxDimensionAVIF(t), "image/avif"); err == nil {
		t.Fatal("AVIF_UINT32_OVERFLOW: maximum dimensions accepted")
	}
}

func TestPixelLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want bool
	}{
		{4096, 4096, true}, {4097, 4096, false},
		{0, 1, false}, {1, 0, false}, {-1, 1, false}, {1, -1, false},
	} {
		if got := dimensions(tc.w, tc.h); got != tc.want {
			t.Errorf("dimensions(%d,%d)=%v want %v", tc.w, tc.h, got, tc.want)
		}
	}
}
