// Response persistence reuses storage/local's fsync-backed content addressing.
// Called by transaction completion/replay; SQL stores only the opaque reference
// and status, never response JSON or secrets. Objects are immutable and may be
// orphaned by a rolled-back SQL transaction; only SQL-referenced objects replay.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/storage/local"
)

var ErrInvalidResponse = errors.New("invalid persisted idempotency response")
var ErrInvalidReference = errors.New("invalid idempotency response reference")

// Response is the final HTTP response, before any bytes reach the socket.
// Repeated header values and arbitrary binary body bytes are preserved.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}
type ResponseStore interface {
	Put(context.Context, Response) (string, error)
	Get(context.Context, string) (Response, error)
}
type LocalResponseStore struct {
	blobs            *local.Store
	maxBodyBytes     int64
	maxEnvelopeBytes int64
}

const maxHeaderBytes = 64 << 10
const referencePrefix = "idempotency:"

type envelope struct {
	Version  int      `json:"version"`
	Response Response `json:"response"`
}

// NewLocalResponseStore uses a private configured root. maxBodyBytes must equal
// the command response admission limit; it is enforced on BOTH Put and Get.
func NewLocalResponseStore(root string, maxBodyBytes int64) (*LocalResponseStore, error) {
	if maxBodyBytes < 1 || maxBodyBytes > (math.MaxInt64-8*maxHeaderBytes-1024)/2 {
		return nil, ErrInvalidResponse
	}
	maxEnvelope := 2*maxBodyBytes + 8*maxHeaderBytes + 1024
	blobs, err := local.New(root, maxEnvelope)
	if err != nil {
		return nil, err
	}
	return &LocalResponseStore{blobs, maxBodyBytes, maxEnvelope}, nil
}
func (s *LocalResponseStore) Put(ctx context.Context, response Response) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil || s.blobs == nil {
		return "", ErrInvalidResponse
	}
	checked, err := validateResponse(response, s.maxBodyBytes)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope{1, checked})
	if err != nil {
		return "", err
	}
	if int64(len(encoded)) > s.maxEnvelopeBytes {
		return "", ErrInvalidResponse
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	return referencePrefix + blob.Key, nil
}
func (s *LocalResponseStore) Get(ctx context.Context, reference string) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if s == nil || s.blobs == nil {
		return Response{}, ErrInvalidResponse
	}
	key, ok := responseKey(reference)
	if !ok {
		return Response{}, ErrInvalidReference
	}
	f, err := s.blobs.Open(key)
	if err != nil {
		return Response{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, s.maxEnvelopeBytes+1))
	if err != nil {
		return Response{}, err
	}
	if int64(len(raw)) > s.maxEnvelopeBytes {
		return Response{}, ErrInvalidResponse
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != strings.Split(key, "/")[2] {
		return Response{}, ErrInvalidResponse
	}
	var saved envelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&saved) != nil || saved.Version != 1 {
		return Response{}, ErrInvalidResponse
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Response{}, ErrInvalidResponse
	}
	return validateResponse(saved.Response, s.maxBodyBytes)
}
func Replay(ctx context.Context, store ResponseStore, reference string, status int) (Response, error) {
	if store == nil {
		return Response{}, ErrInvalidResponse
	}
	response, err := store.Get(ctx, reference)
	if err != nil {
		return Response{}, err
	}
	checked, err := validateResponse(response, int64(len(response.Body)))
	if err != nil || checked.Status != status {
		return Response{}, ErrInvalidResponse
	}
	return checked, nil
}
func responseKey(reference string) (string, bool) {
	if !strings.HasPrefix(reference, referencePrefix) {
		return "", false
	}
	key := strings.TrimPrefix(reference, referencePrefix)
	parts := strings.Split(key, "/")
	if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || len(parts[2]) != 64 || parts[0] != parts[2][:2] || parts[1] != parts[2][2:4] {
		return "", false
	}
	for _, c := range parts[2] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return key, true
}

// Reject unsafe response metadata instead of silently changing the promised
// replay: cookies and hop-by-hop headers are never reusable command results.
func validateResponse(response Response, maxBodyBytes int64) (Response, error) {
	if response.Status < 200 || response.Status > 599 || int64(len(response.Body)) > maxBodyBytes || ((response.Status == 204 || response.Status == 304) && len(response.Body) > 0) {
		return Response{}, ErrInvalidResponse
	}
	var header http.Header
	if response.Header != nil {
		header = make(http.Header, len(response.Header))
	}
	size := 0
	for name, values := range response.Header {
		if !headerName(name) {
			return Response{}, ErrInvalidResponse
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		switch strings.ToLower(name) {
		case "set-cookie", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			return Response{}, ErrInvalidResponse
		}
		if _, duplicate := header[canonical]; duplicate {
			return Response{}, ErrInvalidResponse
		}
		for _, value := range values {
			// net/http serializes one "Name: value\r\n" line per value.
			size += len(name) + len(value) + 4
			if !utf8.ValidString(value) {
				return Response{}, ErrInvalidResponse
			}
			if size > maxHeaderBytes {
				return Response{}, ErrInvalidResponse
			}
			for i := range len(value) {
				if value[i] == '\r' || value[i] == '\n' || value[i] == 127 || value[i] < 32 && value[i] != '\t' {
					return Response{}, ErrInvalidResponse
				}
			}
		}
		var copied []string
		if values != nil {
			copied = make([]string, len(values))
			copy(copied, values)
		}
		header[canonical] = copied
	}
	if size > maxHeaderBytes {
		return Response{}, ErrInvalidResponse
	}
	if values, exists := header["Content-Length"]; exists {
		if len(values) != 1 {
			return Response{}, ErrInvalidResponse
		}
		if values[0] == "" {
			return Response{}, ErrInvalidResponse
		}
		for i := range len(values[0]) {
			if values[0][i] < '0' || values[0][i] > '9' {
				return Response{}, ErrInvalidResponse
			}
		}
		length, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || length != int64(len(response.Body)) {
			return Response{}, ErrInvalidResponse
		}
	}
	return Response{Status: response.Status, Header: header, Body: bytes.Clone(response.Body)}, nil
}
func headerName(name string) bool {
	if len(name) == 0 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
