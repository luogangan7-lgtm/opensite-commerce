// Package contract defines the anonymous current sealed-release read port.
// Dependencies: stdlib; callers: publish service and HTTP transport.
// Invariants: text-only blocks, local canonical paths, exact decimal amount strings.
package contract

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var ErrNotFound = errors.New("published site not found")
var ErrInvalid = errors.New("invalid public read input")
var ErrUnavailable = errors.New("published site unavailable")
var hostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var pathPattern = regexp.MustCompile(`^/(?:[a-zA-Z0-9_-]+(?:/[a-zA-Z0-9_-]+)*)?$`)

func NormalizeHost(v string) (string, error) {
	v = strings.TrimSuffix(strings.ToLower(v), ".")
	if len(v) == 0 || len(v) > 253 {
		return "", ErrInvalid
	}
	for _, label := range strings.Split(v, ".") {
		if !hostLabel.MatchString(label) {
			return "", ErrInvalid
		}
	}
	return v, nil
}
func SafePath(v string) bool { return len(v) <= 512 && pathPattern.MatchString(v) }

// SafeCMSPath reserves generated product and error routes while allowing nested CMS pages.
func SafeCMSPath(v string) bool {
	return SafePath(v) && v != "/404" && v != "/502" && v != "/products" && !strings.HasPrefix(v, "/products/")
}

type Block struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}
type Navigation struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}
type Page struct {
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Blocks      []Block `json:"blocks"`
}
type Product struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	UnitAmount  string `json:"unit_amount"`
	Currency    string `json:"currency"`
}
type Site struct {
	SiteID      string       `json:"site_id"`
	ReleaseID   string       `json:"release_id"`
	Hostname    string       `json:"hostname"`
	Locale      string       `json:"locale"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Navigation  []Navigation `json:"navigation"`
	Pages       []Page       `json:"pages"`
	Products    []Product    `json:"products"`
}
type Reader interface {
	Current(context.Context, string) (Site, error)
}
