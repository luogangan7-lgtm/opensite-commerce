// Package catalog owns product/SKU lifecycle and variant option assignments.
// It depends on sqlc and platform HTTP/errors, and is called by API bootstrap.
// Tenant identity must come from the router; codes are canonical ASCII only.
package catalog

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

func invalid(detail string) error { return apperrors.New(422, "invalid_catalog", detail) }

// Codes permit 1..128 ASCII letters/digits/dot/underscore/hyphen, trimming
// ASCII whitespace and uppercasing ASCII letters. Unicode lookalikes fail.
func normalizeCode(code string) (string, error) {
	code = strings.Trim(code, " \t\r\n\v\f")
	if len(code) < 1 || len(code) > 128 {
		return "", invalid("Code must contain 1 to 128 ASCII letters, digits, ., _ or -")
	}
	for _, c := range []byte(code) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return "", invalid("Code must contain only ASCII letters, digits, ., _ or -")
		}
	}
	return strings.ToUpper(code), nil
}
func parseVersion(value string) (int64, error) {
	if value == "" {
		return 0, apperrors.New(428, "precondition_required", "If-Match is required")
	}
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return 0, apperrors.New(400, "invalid_if_match", "If-Match must be a quoted positive decimal version")
	}
	raw := value[1 : len(value)-1]
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != raw {
		return 0, apperrors.New(400, "invalid_if_match", "If-Match must be a quoted positive decimal version")
	}
	return n, nil
}
func pageLimit(limit *int) (int32, error) {
	if limit == nil {
		return 20, nil
	}
	if *limit < 1 {
		return 0, apperrors.New(400, "invalid_limit", "Limit must be positive")
	}
	if *limit > 100 {
		return 0, apperrors.New(400, "invalid_limit", "Page size must not exceed 100")
	}
	return int32(*limit), nil
}

func lifecycle(value string) error {
	if value != "draft" && value != "ready" {
		return invalid("Lifecycle status must be draft or ready; use archive to archive a resource")
	}
	return nil
}

type cursor struct {
	Time time.Time `json:"t"`
	ID   uuid.UUID `json:"id"`
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	b, _ := json.Marshal(cursor{t, id})
	return base64.RawURLEncoding.EncodeToString(b)
}
func parseCursor(encoded *string) (time.Time, uuid.UUID, bool, error) {
	if encoded == nil {
		return time.Time{}, uuid.Nil, false, nil
	}
	bad := apperrors.New(400, "invalid_cursor", "Cursor is invalid")
	if len(*encoded) > 512 {
		return time.Time{}, uuid.Nil, false, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(*encoded)
	if err != nil {
		return time.Time{}, uuid.Nil, false, bad
	}
	var c cursor
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil || c.Time.IsZero() || c.ID == uuid.Nil || encodeCursor(c.Time, c.ID) != *encoded {
		return time.Time{}, uuid.Nil, false, bad
	}
	return c.Time, c.ID, true, nil
}
