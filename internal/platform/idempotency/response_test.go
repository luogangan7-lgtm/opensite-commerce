package idempotency

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLocalResponseRestartExactReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, e := NewLocalResponseStore(root, 1024)
	if e != nil {
		t.Fatal(e)
	}
	want := Response{Status: 201, Header: http.Header{"Content-Type": {"application/octet-stream"}, "Etag": {"\"v1\""}, "X-Multi": {"first", "second"}}, Body: []byte{0, 255, 'x', '\n'}}
	ref, e := a.Put(ctx, want)
	if e != nil {
		t.Fatal(e)
	}
	again, e := a.Put(ctx, want)
	if e != nil || again != ref {
		t.Fatal("immutable content reference unstable", e)
	}
	b, e := NewLocalResponseStore(root, 1024)
	if e != nil {
		t.Fatal(e)
	}
	got, e := Replay(ctx, b, ref, 201)
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("restart replay:%+v %v", got, e)
	}
	got.Body[0] = 42
	got.Header.Set("Etag", "changed")
	fresh, e := b.Get(ctx, ref)
	if e != nil || !bytes.Equal(fresh.Body, want.Body) || fresh.Header.Get("Etag") != "\"v1\"" {
		t.Fatal("caller mutates persistedresponse", e)
	}
	if _, e = Replay(ctx, b, ref, 202); !errors.Is(e, ErrInvalidResponse) {
		t.Fatalf("SQL/storestatus mismatch accepted:%v", e)
	}
}
func TestLocalResponseValidationAndTamper(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, e := NewLocalResponseStore(root, 8)
	if e != nil {
		t.Fatal(e)
	}
	bad := []Response{{Status: 200, Header: http.Header{"X-Invalid-UTF8": {string([]byte{255})}}}, {Status: 200, Header: http.Header{"Content-Length": {"+1"}}, Body: []byte("x")}, {Status: 200, Header: http.Header{"content-type": {"a"}, "Content-Type": {"b"}}}, {Status: 199}, {Status: 600}, {Status: 200, Body: []byte("123456789")}, {Status: 200, Header: http.Header{"Set-Cookie": {"secret=value"}}}, {Status: 200, Header: http.Header{"Connection": {"close"}}}, {Status: 200, Header: http.Header{"Transfer-Encoding": {"chunked"}}}, {Status: 200, Header: http.Header{"Bad Header": {"x"}}}, {Status: 200, Header: http.Header{"X-Test": {"a\r\nb"}}}, {Status: 200, Header: http.Header{"Content-Length": {"2"}}, Body: []byte("x")}, {Status: 204, Body: []byte("x")}, {Status: 200, Header: http.Header{"X-Large": {strings.Repeat("x", 65537)}}}}
	for i, r := range bad {
		if _, e = s.Put(ctx, r); !errors.Is(e, ErrInvalidResponse) {
			t.Errorf("badresponse%d accepted:%v", i, e)
		}
	}
	for _, ref := range []string{"", "../secret", "idempotency:../../secret", "idempotency:aa/bb/notahash"} {
		if _, e = s.Get(ctx, ref); !errors.Is(e, ErrInvalidReference) {
			t.Errorf("badref accepted:%q %v", ref, e)
		}
	}
	ref, e := s.Put(ctx, Response{Status: 200, Body: []byte("safe")})
	if e != nil {
		t.Fatal(e)
	}
	key := strings.TrimPrefix(ref, "idempotency:")
	if e = os.WriteFile(filepath.Join(root, "originals", filepath.FromSlash(key)), []byte(`{"version":1,"response":{"Status":200,"Body":"eA=="}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, ref); !errors.Is(e, ErrInvalidResponse) {
		t.Fatalf("changed content hash replayed:%v", e)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.Put(canceled, Response{Status: 200}); !errors.Is(e, context.Canceled) {
		t.Fatal("canceled write accepted", e)
	}
}

func TestLocalResponseValidEscapedHeaders(t *testing.T) {
	s, err := NewLocalResponseStore(t.TempDir(), 8)
	if err != nil {
		t.Fatal(err)
	}
	want := Response{Status: 200, Header: http.Header{"X-Escaped": {strings.Repeat("<", 60000)}, "X-Unicode": {"中文"}}, Body: []byte("x")}
	ref, err := s.Put(context.Background(), want)
	if err != nil {
		t.Fatal("valid header envelope rejected", err)
	}
	got, err := s.Get(context.Background(), ref)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("escaped header replay:%+v %v", got, err)
	}
}

func TestLocalResponseEmptyRepresentation(t *testing.T) {
	s, err := NewLocalResponseStore(t.TempDir(), 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []Response{
		{Status: 200},
		{Status: 200, Header: http.Header{}, Body: []byte{}},
		{Status: 200, Header: http.Header{"X-Empty": {}, "X-Nil": nil}},
	} {
		ref, err := s.Put(context.Background(), want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(context.Background(), ref)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("empty response roundtrip:%#v want%#v err%v", got, want, err)
		}
	}
}
