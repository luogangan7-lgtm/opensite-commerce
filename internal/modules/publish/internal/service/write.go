// Responsibilities: strictly validate bounded text-only CMS mutation payloads.
// Dependencies: publish contract and stdlib; callers: publish write facade.
// Invariants: no duplicate/unknown JSON keys, raw markup, route ambiguity or unsupported block kinds.
package service

import (
	"bytes"
	"encoding/json"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

type SavePage struct {
	ID       *string               `json:"id,omitempty"`
	PageKey  string                `json:"page_key"`
	Locale   string                `json:"locale"`
	Path     string                `json:"path"`
	Document contract.PageDocument `json:"document"`
	MediaIDs []string              `json:"media_ids"`
}
type DeletePage struct {
	ID     string `json:"id"`
	Locale string `json:"locale"`
}
type Metadata struct {
	Locale      string `json:"locale"`
	Title       string `json:"title"`
	Description string `json:"description"`
}
type NavigationPayload struct {
	Navigation []contract.Navigation `json:"navigation"`
}
type CMSOperation struct {
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload"`
}
type OperationBatch struct {
	Operations []CMSOperation `json:"operations"`
}
type PublishPayload struct {
	RendererBuildID string `json:"renderer_build_id"`
}

// jsonKeys requires exact JSON tags and rejects duplicates at every object
// level; encoding/json otherwise accepts case aliases and last duplicates.
func jsonKeys(d *json.Decoder, t reflect.Type) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		var fields map[string]reflect.Type
		if t != nil && t.Kind() == reflect.Struct {
			fields = map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name != "" && name != "-" {
					fields[name] = f.Type
				}
			}
		}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return contract.ErrInvalid
			}
			seen[key] = true
			var child reflect.Type
			if fields != nil {
				var exists bool
				child, exists = fields[key]
				if !exists {
					return contract.ErrInvalid
				}
			}
			if err := jsonKeys(d, child); err != nil {
				return err
			}
		}
	case '[':
		var child reflect.Type
		if t != nil && t.Kind() == reflect.Slice && t != reflect.TypeOf(json.RawMessage{}) {
			child = t.Elem()
		}
		for d.More() {
			if err := jsonKeys(d, child); err != nil {
				return err
			}
		}
	default:
		return contract.ErrInvalid
	}
	_, err = d.Token()
	return err
}
func StrictDecode(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > contract.MaxWriteBytes || !utf8.Valid(raw) {
		return contract.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if jsonKeys(d, reflect.TypeOf(v)) != nil {
		return contract.ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return contract.ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return contract.ErrInvalid
	}
	return nil
}
func plain(v string, max int, required bool) bool {
	return text(v, max, required) && !strings.ContainsAny(v, "<>")
}
func MetadataValid(m Metadata) bool {
	_, ok := canonicalLocale(m.Locale)
	return ok && plain(m.Title, 300, true) && plain(m.Description, 2000, false)
}
func NormalizePayload(op string, raw []byte) ([]byte, error) {
	var result any
	switch op {
	case "create_draft", "set_metadata":
		var v Metadata
		if StrictDecode(raw, &v) != nil {
			return nil, contract.ErrInvalid
		}
		v.Title = cleanText(v.Title)
		v.Description = cleanText(v.Description)
		if !MetadataValid(v) {
			return nil, contract.ErrInvalid
		}
		v.Locale, _ = canonicalLocale(v.Locale)
		result = v
	case "save_page", "update_page":
		var v SavePage
		if StrictDecode(raw, &v) != nil {
			return nil, contract.ErrInvalid
		}
		if op == "update_page" && v.ID == nil {
			return nil, contract.ErrInvalid
		}
		v.Document.Title = cleanText(v.Document.Title)
		v.Document.Description = cleanText(v.Document.Description)
		for i := range v.Document.Blocks {
			v.Document.Blocks[i].Text = cleanText(v.Document.Blocks[i].Text)
		}
		if (v.ID != nil && !tenanttx.ValidTenantID(*v.ID)) || !plain(v.PageKey, 128, true) || !contract.SafeCMSPath(v.Path) || !MetadataValid(Metadata{v.Locale, v.Document.Title, v.Document.Description}) || v.Document.Blocks == nil || len(v.Document.Blocks) > 200 || v.MediaIDs == nil || len(v.MediaIDs) > 100 {
			return nil, contract.ErrInvalid
		}
		for _, b := range v.Document.Blocks {
			if (b.Kind != "heading" && b.Kind != "paragraph") || !plain(b.Text, 10000, true) {
				return nil, contract.ErrInvalid
			}
		}
		seen := map[string]bool{}
		for _, id := range v.MediaIDs {
			if !tenanttx.ValidTenantID(id) || seen[id] {
				return nil, contract.ErrInvalid
			}
			seen[id] = true
		}
		v.Locale, _ = canonicalLocale(v.Locale)
		result = v
	case "delete_page":
		var v DeletePage
		if StrictDecode(raw, &v) != nil || !tenanttx.ValidTenantID(v.ID) {
			return nil, contract.ErrInvalid
		}
		canonical, ok := canonicalLocale(v.Locale)
		if !ok {
			return nil, contract.ErrInvalid
		}
		v.Locale = canonical
		result = v
	case "set_navigation":
		var v NavigationPayload
		if StrictDecode(raw, &v) != nil || v.Navigation == nil || len(v.Navigation) > 50 {
			return nil, contract.ErrInvalid
		}
		for i := range v.Navigation {
			v.Navigation[i].Label = cleanText(v.Navigation[i].Label)
			n := v.Navigation[i]
			if !plain(n.Label, 200, true) || !contract.SafePath(n.Href) {
				return nil, contract.ErrInvalid
			}
		}
		result = v
	case "apply_operations":
		var v OperationBatch
		if StrictDecode(raw, &v) != nil || len(v.Operations) == 0 || len(v.Operations) > 100 {
			return nil, contract.ErrInvalid
		}
		for i, o := range v.Operations {
			if o.Operation != "save_page" && o.Operation != "delete_page" && o.Operation != "set_navigation" && o.Operation != "set_metadata" {
				return nil, contract.ErrInvalid
			}
			p, err := NormalizePayload(o.Operation, o.Payload)
			if err != nil {
				return nil, err
			}
			v.Operations[i].Payload = p
		}
		result = v
	case "publish_draft":
		var v PublishPayload
		if StrictDecode(raw, &v) != nil || !plain(v.RendererBuildID, 128, true) {
			return nil, contract.ErrInvalid
		}
		result = v
	default:
		return nil, contract.ErrInvalid
	}
	canonical, err := json.Marshal(result)
	if err != nil || len(canonical) > contract.MaxWriteBytes {
		return nil, contract.ErrInvalid
	}
	return canonical, nil
}

// DraftSite uses the exact published renderer projection, including navigation
// resolution and combined response budget. Draft-only metadata never reaches it.
// BudgetHost is the longest admissible public hostname (253 bytes). Publication
// validation projects with it so a later domain attachment cannot exceed 1 MiB.
var BudgetHost = strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("a", 61)

// DraftSite projects a draft; host is "" for preview and BudgetHost for validation.
func DraftSite(d contract.Draft, host string) (contract.Site, error) {
	pages := make([]contract.Page, 0, len(d.Pages))
	seenIDs := map[string]bool{}
	for _, p := range d.Pages {
		if !tenanttx.ValidTenantID(p.ID) || seenIDs[p.ID] || p.Locale != d.Locale {
			return contract.Site{}, contract.ErrUnavailable
		}
		seenIDs[p.ID] = true
		pages = append(pages, contract.Page{Path: p.Path, Title: p.Document.Title, Description: p.Document.Description, Blocks: p.Document.Blocks})
	}
	manifestBytes, err := json.Marshal(manifest{Version: 1, Locale: d.Locale, Title: d.Title, Description: d.Description, Navigation: d.Navigation, Products: d.Products})
	if err != nil {
		return contract.Site{}, contract.ErrUnavailable
	}
	pageBytes, err := json.Marshal(pages)
	if err != nil {
		return contract.Site{}, contract.ErrUnavailable
	}
	return Project(d.SiteID, d.DraftID, host, d.Locale, manifestBytes, pageBytes)
}

// DecodeSnapshot admits only the bounded, typed output of controlled SQL routines.
func DecodeSnapshot(raw []byte, v any) error { return decode(raw, v) }
