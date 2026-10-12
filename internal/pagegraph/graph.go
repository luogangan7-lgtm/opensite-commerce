// Package pagegraph validates the canonical, data-only PageDocument model.
// It deliberately does not execute scripts, resolvers or client-defined SQL.
package pagegraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	MaxNodes = 200
	MaxDepth = 8
)

type Binding struct {
	Resolver string          `json:"resolver"`
	Params   json.RawMessage `json:"params"`
}

type Node struct {
	Component string              `json:"component"`
	Props     json.RawMessage     `json:"props"`
	Binding   *Binding            `json:"binding,omitempty"`
	Slots     map[string][]string `json:"slots"`
}

// UnmarshalJSON keeps unknown-field rejection and rejects explicit nulls where
// api/openapi.yaml ComponentNode requires an object or array: decoding null
// into a pointer/map/slice is otherwise indistinguishable from "absent"/empty.
func (n *Node) UnmarshalJSON(b []byte) error {
	type alias Node
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var a alias
	if err := d.Decode(&a); err != nil {
		return err
	}
	var raw struct {
		Props   json.RawMessage            `json:"props"`
		Binding json.RawMessage            `json:"binding"`
		Slots   map[string]json.RawMessage `json:"slots"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	isNull := func(m json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(m), []byte("null")) }
	if isNull(raw.Props) || isNull(raw.Binding) {
		return errors.New("props/binding must not be null")
	}
	for slot, v := range raw.Slots {
		if isNull(v) {
			return fmt.Errorf("slot %q must be an array", slot)
		}
	}
	*n = Node(a)
	return nil
}

type Document struct {
	SchemaVersion int             `json:"schema_version"`
	RootID        string          `json:"root_id"`
	Nodes         map[string]Node `json:"nodes"`
}

// Registry is a finite set of components bundled in the trusted renderer.
// Adding a component requires a code deployment, not a page JSON edit.
var Registry = map[string]map[string]bool{
	"core.page-layout@1":     {"main": true},
	"core.section@1":         {"content": true},
	"core.hero@1":            {},
	"catalog.product-grid@1": {},
	"company.profile@1":      {},
}

var Resolvers = map[string]bool{
	"catalog.featured-products@1":   true,
	"catalog.collection-products@1": true,
	"company.profile@1":             true,
}

func ValidateJSON(src []byte) error {
	d := json.NewDecoder(strings.NewReader(string(src)))
	d.DisallowUnknownFields()
	var doc Document
	if err := d.Decode(&doc); err != nil {
		return fmt.Errorf("parse document: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing document data")
	}
	return Validate(doc)
}

func Validate(doc Document) error {
	if doc.SchemaVersion != 1 {
		return errors.New("unsupported schema_version")
	}
	if len(doc.Nodes) == 0 || len(doc.Nodes) > MaxNodes {
		return fmt.Errorf("node count must be in [1,%d]", MaxNodes)
	}
	if doc.RootID == "" {
		return errors.New("root_id required")
	}
	root, ok := doc.Nodes[doc.RootID]
	if !ok || root.Component != "core.page-layout@1" {
		return errors.New("root must be an existing core.page-layout@1")
	}
	parents := map[string]int{}
	ids := make([]string, 0, len(doc.Nodes))
	for id := range doc.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := doc.Nodes[id]
		if id == "" || len(id) > 128 {
			return fmt.Errorf("invalid node id %q", id)
		}
		allowedSlots, ok := Registry[node.Component]
		if !ok {
			return fmt.Errorf("node %s: unregistered component %q", id, node.Component)
		}
		if err := validateProps(id, node); err != nil {
			return err
		}
		if node.Slots == nil { // ComponentNode.slots is a required object in api/openapi.yaml
			return fmt.Errorf("node %s: slots object required", id)
		}
		if len(node.Slots) > len(allowedSlots) {
			return fmt.Errorf("node %s: unsupported slot", id)
		}
		for slot, children := range node.Slots {
			if !allowedSlots[slot] {
				return fmt.Errorf("node %s: unsupported slot %q", id, slot)
			}
			for _, child := range children {
				if _, found := doc.Nodes[child]; !found {
					return fmt.Errorf("node %s: child %q does not exist", id, child)
				}
				parents[child]++
				if parents[child] > 1 {
					return fmt.Errorf("node %s has multiple parents", child)
				}
			}
		}
	}
	if parents[doc.RootID] != 0 {
		return errors.New("root cannot have a parent")
	}
	for _, id := range ids {
		if id != doc.RootID && parents[id] != 1 {
			return fmt.Errorf("node %s disconnected", id)
		}
	}
	state := make(map[string]int)
	var walk func(string, int) error
	walk = func(id string, depth int) error {
		if depth > MaxDepth {
			return fmt.Errorf("max tree depth %d exceeded", MaxDepth)
		}
		if state[id] == 1 {
			return fmt.Errorf("cycle at %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		node := doc.Nodes[id]
		for _, kids := range node.Slots {
			for _, k := range kids {
				if err := walk(k, depth+1); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		return nil
	}
	if err := walk(doc.RootID, 1); err != nil {
		return err
	}
	if len(state) != len(doc.Nodes) {
		return errors.New("unreachable nodes")
	}
	return nil
}

func validateProps(id string, n Node) error {
	var props map[string]json.RawMessage
	if len(n.Props) == 0 {
		return fmt.Errorf("node %s: props object required", id)
	}
	if err := json.Unmarshal(n.Props, &props); err != nil || props == nil {
		return fmt.Errorf("node %s: props must be an object", id)
	}
	allowed := map[string]bool{}
	switch n.Component {
	case "core.page-layout@1", "core.section@1", "company.profile@1":
		if n.Binding != nil && n.Component != "company.profile@1" {
			return fmt.Errorf("node %s: binding not supported", id)
		}
	case "core.hero@1":
		allowed = map[string]bool{"title": true, "image_id": true, "layout": true}
		if err := requireString(props, "title", 1, 120); err != nil {
			return fmt.Errorf("node %s: %w", id, err)
		}
		if _, ok := props["layout"]; ok {
			var layout string
			if json.Unmarshal(props["layout"], &layout) != nil || (layout != "split" && layout != "full" && layout != "contained") {
				return fmt.Errorf("node %s: invalid hero layout", id)
			}
		}
	case "catalog.product-grid@1":
		allowed = map[string]bool{"columns_desktop": true, "columns_mobile": true, "show_price": true}
		if err := requireInt(props, "columns_desktop", 1, 6); err != nil {
			return fmt.Errorf("node %s: %w", id, err)
		}
		if err := requireInt(props, "columns_mobile", 1, 2); err != nil {
			return fmt.Errorf("node %s: %w", id, err)
		}
		if n.Binding == nil || !Resolvers[n.Binding.Resolver] || !strings.HasPrefix(n.Binding.Resolver, "catalog.") {
			return fmt.Errorf("node %s: missing or unregistered catalog binding", id)
		}
		var params struct {
			Limit int `json:"limit"`
		}
		if json.Unmarshal(n.Binding.Params, &params) != nil || params.Limit < 1 || params.Limit > 48 {
			return fmt.Errorf("node %s: limit must be in [1,48]", id)
		}
		if raw, ok := props["show_price"]; ok {
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return fmt.Errorf("node %s: show_price must be boolean", id)
			}
		}
	}
	for key := range props {
		if !allowed[key] {
			return fmt.Errorf("node %s: unsupported prop %q", id, key)
		}
	}
	if n.Binding != nil && n.Component == "company.profile@1" {
		if n.Binding.Resolver != "company.profile@1" {
			return fmt.Errorf("node %s: invalid company resolver", id)
		}
		// DataBinding.params is a required object in api/openapi.yaml.
		var params map[string]json.RawMessage
		if json.Unmarshal(n.Binding.Params, &params) != nil || params == nil {
			return fmt.Errorf("node %s: binding params must be an object", id)
		}
	}
	if n.Binding != nil && n.Component != "catalog.product-grid@1" && n.Component != "company.profile@1" {
		return fmt.Errorf("node %s: binding not allowed", id)
	}
	return nil
}

func requireString(props map[string]json.RawMessage, key string, min, max int) error {
	raw, ok := props[key]
	if !ok {
		return fmt.Errorf("%s required", key)
	}
	var v string
	if json.Unmarshal(raw, &v) != nil || len([]rune(v)) < min || len([]rune(v)) > max {
		return fmt.Errorf("%s length invalid", key)
	}
	return nil
}
func requireInt(props map[string]json.RawMessage, key string, min, max int) error {
	raw, ok := props[key]
	if !ok {
		return fmt.Errorf("%s required", key)
	}
	var v int
	if json.Unmarshal(raw, &v) != nil || v < min || v > max {
		return fmt.Errorf("%s must be in [%d,%d]", key, min, max)
	}
	return nil
}
