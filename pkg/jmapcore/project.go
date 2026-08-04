package jmapcore

import (
	"reflect"
	"strings"
	"sync"
)

// Project returns the object's requested properties as a map ready to be
// marshalled (RFC 8620 §5.1).
//
// Without it a response carries every field of the object regardless of the
// request, which is a deviation from the specification and, worse, a silent
// misstatement: a field the server never computed marshals as its zero value
// and is indistinguishable from one computed as empty. A client asking for a
// subject would be told, as a fact, that the message has no attachments.
//
// props nil means the client named none, which asks for all of them; the value
// is returned unchanged so it marshals as itself.
//
// A map of values rather than a json.Marshaler returning bytes: encoding/json
// validates and compacts whatever a Marshaler hands back, so a projection built
// that way re-parses everything it just encoded — on a message whose body the
// client did ask for, that is a second pass over the body. Selecting the fields
// and letting the encoder run once avoids both that and encoding the fields
// nobody asked for.
func Project(value any, props []string) any {
	if props == nil {
		return value
	}
	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return value
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return value
	}

	fields := jsonFields(v.Type())
	out := make(map[string]any, len(props)+len(alwaysProjected))
	add := func(name string) {
		idx, ok := fields[name]
		if !ok {
			// Unknown property names are rejected when the request is
			// validated, and a property this server does not implement is
			// absent rather than null.
			return
		}
		out[name] = v.Field(idx).Interface()
	}
	for _, name := range alwaysProjected {
		add(name)
	}
	for _, name := range props {
		add(name)
	}
	return out
}

// alwaysProjected are returned whatever the client asked for: the id is what
// every other answer is keyed by, and §5.1 requires it.
var alwaysProjected = [...]string{"id"}

// fieldCache maps a struct type to its JSON name → field index.
var fieldCache sync.Map // reflect.Type → map[string]int

// jsonFields resolves a struct's JSON names once per type.
func jsonFields(t reflect.Type) map[string]int {
	if cached, ok := fieldCache.Load(t); ok {
		if fields, ok := cached.(map[string]int); ok {
			return fields
		}
	}
	fields := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		fields[name] = i
	}
	fieldCache.Store(t, fields)
	return fields
}
