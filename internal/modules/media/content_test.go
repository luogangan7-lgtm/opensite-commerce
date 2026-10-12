// Media boundary tests use actual image bytes and reject hostile inputs.
package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 2, 3))
	im.Set(0, 0, color.RGBA{255, 0, 0, 255})
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestContentBoundary(t *testing.T) {
	good := pngBytes(t)
	cases := []struct {
		name string
		data []byte
		mime string
		want bool
	}{{"valid", good, "image/png", true}, {"type mismatch", good, "image/jpeg", false}, {"fake PNG", []byte("\x89PNG\r\n\x1a\nfake"), "image/png", false}, {"SVG", []byte("<svg></svg>"), "image/svg+xml", false}, {"fake webp", []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake"), "image/webp", false}, {"fake avif", []byte("\x00\x00\x00\x18ftypavif\x00\x00\x00\x00avifmif1"), "image/avif", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := inspect(c.data, c.mime)
			if (err == nil) != c.want {
				t.Fatalf("accepted=%v want=%v: %v", err == nil, c.want, err)
			}
		})
	}
}
func TestJPEGDecoded(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil); err != nil {
		t.Fatal(err)
	}
	mime, w, h, err := inspect(b.Bytes(), "image/jpeg")
	if err != nil || mime != "image/jpeg" || w != 2 || h != 3 {
		t.Fatalf("jpeg dimensions=%dx%d mime=%s err=%v", w, h, mime, err)
	}
	if _, _, _, err := inspect(b.Bytes()[:len(b.Bytes())/2], "image/jpeg"); err == nil {
		t.Fatal("truncated JPEG accepted")
	}
}
func TestModernImageContainers(t *testing.T) {
	for _, kind := range []string{"webp", "avif"} {
		t.Run(kind, func(t *testing.T) {
			b, err := os.ReadFile("testdata/tiny." + kind)
			if err != nil {
				t.Fatal(err)
			}
			mime, w, h, err := inspect(b, "image/"+kind)
			if err != nil || mime != "image/"+kind || w != 2 || h != 3 {
				t.Fatalf("dimensions=%dx%d mime=%s err=%v", w, h, mime, err)
			}
			if _, _, _, err := inspect(b[:len(b)/2], "image/"+kind); err == nil {
				t.Fatal("truncated container accepted")
			}
		})
	}
}
func TestFilenameBoundary(t *testing.T) {
	for _, n := range []string{"../x.png", "a/b.png", "a\\b.png", "..", ".", "x\x00.png", ""} {
		if validFilename(n) {
			t.Fatalf("unsafe filename accepted %q", n)
		}
	}
	if !validFilename("photo.png") {
		t.Fatal("safe name rejected")
	}
}
