// Responsibilities: admit one bounded JSON DTO object at the HTTP boundary.
// Dependencies: stdlib MIME/HTTP/JSON; platform/errors; RouteSpec body limit.
// Callers: transport handlers; owns no tables and performs no domain validation.
// Invariants: only application/json; bounded body/depth/decoded strings;
// unique decoded object keys, strict DTO fields and one JSON value. Callers
// must discard dst after any error.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

const (
	// Root object counts as depth one; every nested object/array adds one.
	maxJSONDepth = 64
	// Length is decoded UTF-8 bytes, including object keys, not wire bytes/runes.
	maxJSONStringBytes = 64 << 10
)

// DecodeJSON validates transport and JSON limits before decoding a DTO.
// Standard decoding may partially modify dst on a type/unknown-field error.
// ponytail: custom UnmarshalJSON methods must enforce their own field strictness;
// generated DTOs should use ordinary structs to inherit DisallowUnknownFields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return apperrors.New(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	}
	limit := maxBodySize(r)
	if limit <= 0 {
		limit = DefaultMaxBodySize
	}
	if r.Body == nil {
		return invalidJSON()
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperrors.New(http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds the route limit")
		}
		return invalidJSON()
	}
	if err := checkJSONLimits(body); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalidJSON()
	}
	return nil
}

func invalidJSON() error {
	// Parser diagnostics can contain request content; never expose them.
	return apperrors.New(http.StatusBadRequest, "invalid_json", "Body must be one valid JSON object within the JSON limits")
}

func checkJSONLimits(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return invalidJSON()
	}
	// Arrays have nil keys; each object has its own decoded-name set.
	type frame struct {
		keys      map[string]struct{}
		expectKey bool
	}
	stack := []frame{{keys: make(map[string]struct{}), expectKey: true}}
	for len(stack) > 0 {
		token, err := dec.Token()
		if err != nil {
			return invalidJSON()
		}
		current := &stack[len(stack)-1]
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				if len(stack) == maxJSONDepth {
					return invalidJSON()
				}
				// Opening a container consumes its parent's value; only the
				// new top frame sees tokens until that container closes.
				current.expectKey = true
				next := frame{}
				if value == '{' {
					next = frame{keys: make(map[string]struct{}), expectKey: true}
				}
				stack = append(stack, next)
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		case string:
			if len(value) > maxJSONStringBytes {
				return invalidJSON()
			}
			if current.keys != nil && current.expectKey {
				if _, exists := current.keys[value]; exists {
					return invalidJSON()
				}
				current.keys[value] = struct{}{}
				current.expectKey = false
			} else {
				current.expectKey = true
			}
		default:
			// Numbers, booleans and null finish an object value as well.
			current.expectKey = true
		}
	}
	// Token also detects trailing junk. EOF after whitespace proves one value.
	if _, err := dec.Token(); err != io.EOF {
		return invalidJSON()
	}
	return nil
}
