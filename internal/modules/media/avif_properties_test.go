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
	next := makeBox("iprp", append(makeBox("ipco", properties), makeBox("ipma", association)...))
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
	for _, tc := range []struct {
		name         string
		associations []byte
	}{
		{"narrow version0", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 4, 1, 2, 0x83, 4}},
		{"wide version0", []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 4, 0, 1, 0, 2, 0x80, 3, 0, 4}},
		{"wide version1", []byte{1, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 4, 0, 1, 0, 2, 0x80, 3, 0, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, h, err := inspect(avifWithProperties(t, props, tc.associations), "image/avif")
			if err != nil || w != 2 || h != 3 {
				t.Fatalf("primary dimensions=%dx%d err=%v", w, h, err)
			}
		})
	}
}
