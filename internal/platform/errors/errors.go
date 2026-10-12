// Package errors carries public domain failures to the HTTP mapper. It has no
// dependencies or tables; domain services construct it and httpapi consumes it.
// Only the HTTP mapper decides whether a detail is safe to expose.
package errors

// Error describes a domain failure without depending on the HTTP transport.
type Error struct {
	Status int
	Code   string
	Detail string
}

// New constructs a failure; the transport validates status before publishing it.
func New(status int, code, detail string) *Error {
	return &Error{Status: status, Code: code, Detail: detail}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail != "" {
		return e.Detail
	}
	return e.Code
}
