// Content validation depends only on standard image/container parsers; HTTP
// handlers call it before LocalFS writes. A filename is metadata, never a path.
package media

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
	"unicode"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

const MaxBytes int64 = 10 << 20
const maxPixels = 16 << 20

// validFilename rejects path separators, every Unicode control character (Cc:
// C0, DEL, C1) and bidi override/isolate marks (U+202A-202E, U+2066-2069), which
// can visually spoof extensions in admin UIs and logs.
func validFilename(s string) bool {
	if s == "" || len(s) > 255 || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 {
			return false
		}
	}
	return true
}
func allowedMIME(s string) bool {
	switch s {
	case "image/jpeg", "image/png", "image/webp", "image/avif":
		return true
	}
	return false
}
func dimensions(w, h int) bool { return w > 0 && h > 0 && int64(w)*int64(h) <= maxPixels }
func inspect(b []byte, declared string) (string, int, int, error) {
	bad := func() (string, int, int, error) {
		return "", 0, 0, apperrors.New(415, "invalid_media", "Content is not a supported valid image")
	}
	var mime string
	var w, h int
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case len(b) >= 3 && b[0] == 255 && b[1] == 216 && b[2] == 255:
		mime = "image/jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		mime = "image/webp"
		w, h = webpDimensions(b)
		if !dimensions(w, h) {
			return bad()
		}
	case len(b) >= 16 && string(b[4:8]) == "ftyp":
		mime = "image/avif"
		w, h = avifDimensions(b)
		if !dimensions(w, h) {
			return bad()
		}
	default:
		return bad()
	}
	if declared != "" && declared != mime {
		return "", 0, 0, apperrors.New(415, "mime_mismatch", "Declared MIME does not match image content")
	}
	if mime == "image/png" || mime == "image/jpeg" {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil || !dimensions(cfg.Width, cfg.Height) || "image/"+format != mime {
			return bad()
		}
		// Decode, rather than trusting only headers, rejects truncated/malformed data.
		im, _, err := image.Decode(bytes.NewReader(b))
		if err != nil {
			return bad()
		}
		w, h = im.Bounds().Dx(), im.Bounds().Dy()
	}
	return mime, w, h, nil
}
func le24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }

// WebP has no standard-library decoder. Validate RIFF bounds, frame signature,
// chunk multiplicity, and dimensions; never treat a RIFF header alone as media.
func webpDimensions(b []byte) (int, int) {
	if len(b) < 20 || uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return 0, 0
	}
	w, h, frames := 0, 0, 0
	cw, ch := 0, 0
	for p := 12; p < len(b); {
		if p+8 > len(b) {
			return 0, 0
		}
		n := uint64(binary.LittleEndian.Uint32(b[p+4 : p+8]))
		end := uint64(p) + 8 + n
		if end > uint64(len(b)) {
			return 0, 0
		}
		v := b[p+8 : int(end)]
		kind := string(b[p : p+4])
		switch kind {
		case "VP8X":
			if p != 12 || len(v) != 10 || v[0]&0xc3 != 0 || v[1] != 0 || v[2] != 0 || v[3] != 0 {
				return 0, 0
			}
			cw, ch = 1+le24(v[4:7]), 1+le24(v[7:10])
			if v[0]&2 != 0 {
				return 0, 0
			} // no animated uploads
		case "VP8 ":
			if len(v) < 11 || v[0]&1 != 0 || v[0]&16 == 0 || !bytes.Equal(v[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return 0, 0
			}
			firstPartition := uint32(v[0])>>5 | uint32(v[1])<<3 | uint32(v[2])<<11
			if firstPartition == 0 || uint64(firstPartition)+10 > uint64(len(v)) {
				return 0, 0
			}
			w, h = int(binary.LittleEndian.Uint16(v[6:8])&0x3fff), int(binary.LittleEndian.Uint16(v[8:10])&0x3fff)
			frames++
		case "VP8L":
			if len(v) < 6 || v[0] != 0x2f || v[4]&0xe0 != 0 {
				return 0, 0
			}
			bits := binary.LittleEndian.Uint32(v[1:5])
			w, h = int(bits&0x3fff)+1, int((bits>>14)&0x3fff)+1
			frames++
		case "ANIM", "ANMF":
			return 0, 0
		case "ALPH", "ICCP", "EXIF", "XMP ":
			if cw == 0 {
				return 0, 0
			}
		default:
			return 0, 0
		}
		next := int(end) + int(n&1)
		if next > len(b) || n&1 != 0 && b[int(end)] != 0 {
			return 0, 0
		}
		p = next
	}
	if frames != 1 || cw != 0 && (cw != w || ch != h) {
		return 0, 0
	}
	return w, h
}

type box struct {
	kind       string
	data       []byte
	start, end int
}

func boxes(b []byte, offset int) ([]box, bool) {
	var out []box
	for p := 0; p < len(b); {
		if len(out) > 512 || p+8 > len(b) {
			return nil, false
		}
		n := uint64(binary.BigEndian.Uint32(b[p : p+4]))
		header := 8
		if n == 1 {
			if p+16 > len(b) {
				return nil, false
			}
			n = binary.BigEndian.Uint64(b[p+8 : p+16])
			header = 16
		}
		if n < uint64(header) || n > uint64(len(b)-p) {
			return nil, false
		}
		out = append(out, box{string(b[p+4 : p+8]), b[p+header : p+int(n)], offset + p + header, offset + p + int(n)})
		p += int(n)
	}
	return out, true
}

// AVIF validates bounded ISO-BMFF boxes and an in-file primary image extent,
// plus required AV1 configuration and dimensions. No external data references,
// animation, oversized canvases, or arbitrary header-only ftyp files are accepted.
func avifDimensions(b []byte) (int, int) {
	top, ok := boxes(b, 0)
	if !ok || len(top) < 3 || top[0].kind != "ftyp" {
		return 0, 0
	}
	f := top[0].data
	if len(f) < 12 || len(f)%4 != 0 || string(f[:4]) != "avif" {
		return 0, 0
	}
	var meta box
	var mdats []box
	for _, x := range top {
		if x.kind == "meta" {
			if meta.data != nil {
				return 0, 0
			}
			meta = x
		}
		if x.kind == "mdat" && len(x.data) > 0 {
			mdats = append(mdats, x)
		}
	}
	if len(meta.data) < 4 || len(mdats) == 0 || binary.BigEndian.Uint32(meta.data[:4]) != 0 {
		return 0, 0
	}
	children, ok := boxes(meta.data[4:], meta.start+4)
	if !ok {
		return 0, 0
	}
	primary := uint32(0)
	var iloc, iinf, iprp []byte
	pict := false
	for _, x := range children {
		switch x.kind {
		case "hdlr":
			pict = len(x.data) >= 12 && string(x.data[8:12]) == "pict"
		case "pitm":
			if primary != 0 {
				return 0, 0
			}
			if len(x.data) == 6 && x.data[0] == 0 {
				primary = uint32(binary.BigEndian.Uint16(x.data[4:6]))
			} else if len(x.data) == 8 && x.data[0] == 1 {
				primary = binary.BigEndian.Uint32(x.data[4:8])
			} else {
				return 0, 0
			}
		case "iloc":
			if iloc != nil {
				return 0, 0
			}
			iloc = x.data
		case "iinf":
			if iinf != nil {
				return 0, 0
			}
			iinf = x.data
		case "iprp":
			if iprp != nil {
				return 0, 0
			}
			iprp = x.data
		}
	}
	if !pict || primary == 0 || !avifPrimaryInfo(iinf, primary) || !avifExtent(iloc, primary, mdats) {
		return 0, 0
	}
	return avifPrimaryProperties(iprp, primary)
}

// Properties are one-based in ipco. ipma is the only authority for which
// properties apply to the primary image; thumbnails may have their own ispe.
func avifPrimaryProperties(b []byte, primary uint32) (int, int) {
	xs, ok := boxes(b, 0)
	if !ok {
		return 0, 0
	}
	var props []box
	var ipma []byte
	for _, x := range xs {
		switch x.kind {
		case "ipco":
			if props != nil {
				return 0, 0
			}
			props, ok = boxes(x.data, 0)
			if !ok {
				return 0, 0
			}
		case "ipma":
			if ipma != nil {
				return 0, 0
			}
			ipma = x.data
		}
	}
	if len(props) == 0 || len(ipma) < 8 || ipma[0] > 1 || ipma[1] != 0 || ipma[2] != 0 || ipma[3] > 1 {
		return 0, 0
	}
	count := binary.BigEndian.Uint32(ipma[4:8])
	if count == 0 || count > 512 {
		return 0, 0
	}
	pos := 8
	seenItems := map[uint32]bool{}
	w, h, av1 := 0, 0, false
	for range count {
		itemWidth := 2
		if ipma[0] == 1 {
			itemWidth = 4
		}
		if pos+itemWidth+1 > len(ipma) {
			return 0, 0
		}
		id := uint32(binary.BigEndian.Uint16(ipma[pos : pos+2]))
		if itemWidth == 4 {
			id = binary.BigEndian.Uint32(ipma[pos : pos+4])
		}
		pos += itemWidth
		if id == 0 || seenItems[id] {
			return 0, 0
		}
		seenItems[id] = true
		n := int(ipma[pos])
		pos++
		seenProps := map[int]bool{}
		for range n {
			width := 1
			if ipma[3] == 1 {
				width = 2
			}
			if pos+width > len(ipma) {
				return 0, 0
			}
			index := int(ipma[pos] & 0x7f)
			if width == 2 {
				index = int(binary.BigEndian.Uint16(ipma[pos:pos+2]) & 0x7fff)
			}
			pos += width
			// Zero means no association; even a non-primary item must not
			// reference a property outside the container's bounded table.
			if index == 0 {
				continue
			}
			if index > len(props) || seenProps[index] {
				return 0, 0
			}
			seenProps[index] = true
			if id != primary {
				continue
			}
			p := props[index-1]
			switch p.kind {
			case "ispe":
				if w != 0 || len(p.data) != 12 || binary.BigEndian.Uint32(p.data[:4]) != 0 {
					return 0, 0
				}
				w, h = int(binary.BigEndian.Uint32(p.data[4:8])), int(binary.BigEndian.Uint32(p.data[8:12]))
				if w == 0 || h == 0 {
					return 0, 0
				}
			case "av1C":
				if av1 || len(p.data) < 4 || p.data[0] != 0x81 || p.data[3]&0xe0 != 0 {
					return 0, 0
				}
				av1 = true
			}
		}
	}
	if pos != len(ipma) || !seenItems[primary] || !av1 {
		return 0, 0
	}
	return w, h
}

func avifPrimaryInfo(b []byte, primary uint32) bool {
	if len(b) < 6 {
		return false
	}
	header := 6
	count := uint32(binary.BigEndian.Uint16(b[4:6]))
	if b[0] > 0 {
		header = 8
		if len(b) < header {
			return false
		}
		count = binary.BigEndian.Uint32(b[4:8])
	}
	xs, ok := boxes(b[header:], 0)
	if !ok || uint32(len(xs)) != count {
		return false
	}
	for _, x := range xs {
		if x.kind != "infe" || len(x.data) < 13 {
			continue
		}
		var id uint32
		pos := 0
		if x.data[0] == 2 {
			id = uint32(binary.BigEndian.Uint16(x.data[4:6]))
			pos = 8
		} else if x.data[0] == 3 && len(x.data) >= 15 {
			id = binary.BigEndian.Uint32(x.data[4:8])
			pos = 10
		}
		if id == primary {
			return binary.BigEndian.Uint16(x.data[pos-2:pos]) == 0 && string(x.data[pos:pos+4]) == "av01" && bytes.IndexByte(x.data[pos+4:], 0) >= 0
		}
	}
	return false
}
func avifExtent(b []byte, primary uint32, mdats []box) bool {
	if len(b) < 8 || b[0] > 2 {
		return false
	}
	version := b[0]
	offSize, lenSize, baseSize, indexSize := int(b[4]>>4), int(b[4]&15), int(b[5]>>4), int(b[5]&15)
	if offSize > 8 || lenSize > 8 || baseSize > 8 || indexSize > 8 {
		return false
	}
	p := 6
	read := func(n int) (uint64, bool) {
		if p+n > len(b) {
			return 0, false
		}
		var v uint64
		for _, c := range b[p : p+n] {
			v = v<<8 | uint64(c)
		}
		p += n
		return v, true
	}
	countBytes := 2
	if version == 2 {
		countBytes = 4
	}
	count, ok := read(countBytes)
	if !ok || count > 512 {
		return false
	}
	found := false
	for i := uint64(0); i < count; i++ {
		idBytes := 2
		if version == 2 {
			idBytes = 4
		}
		id, ok := read(idBytes)
		if !ok {
			return false
		}
		if version > 0 {
			method, ok := read(2)
			if !ok || method != 0 {
				return false
			}
		}
		ref, ok := read(2)
		if !ok || ref != 0 {
			return false
		}
		base, ok := read(baseSize)
		if !ok {
			return false
		}
		extents, ok := read(2)
		if !ok || extents == 0 || extents > 512 {
			return false
		}
		for j := uint64(0); j < extents; j++ {
			if version > 0 && indexSize > 0 {
				if _, ok := read(indexSize); !ok {
					return false
				}
			}
			off, ok := read(offSize)
			if !ok {
				return false
			}
			n, ok := read(lenSize)
			if !ok || n == 0 || base+off < base || base+off+n < base+off {
				return false
			}
			contained := false
			for _, m := range mdats {
				if base+off >= uint64(m.start) && base+off+n <= uint64(m.end) {
					contained = true
				}
			}
			if !contained {
				return false
			}
			if id == uint64(primary) {
				found = true
			}
		}
	}
	return found && p == len(b)
}
