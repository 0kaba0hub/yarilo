package jmapcore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ResultReference is a back-reference (RFC 8620 §3.7): an argument whose value
// is taken from an earlier call's result, so a client can chain calls in one
// request instead of waiting for a round trip.
type ResultReference struct {
	// ResultOf is the callId of the earlier invocation.
	ResultOf string `json:"resultOf"`
	// Name is the method name that invocation must have responded with. A
	// client states it so a call answered by "error" cannot be mistaken for a
	// result.
	Name string `json:"name"`
	// Path is a JSON Pointer (RFC 6901) into that result, extended by "*".
	Path string `json:"path"`
}

// resolveBackRefs replaces every "#name" argument with the value its reference
// points at. Results are the responses produced so far, in order, which is what
// makes a forward reference unresolvable by construction.
func resolveBackRefs(args json.RawMessage, done []Invocation) (json.RawMessage, *MethodError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return nil, &MethodError{Type: ErrInvalidArguments, Description: err.Error()}
	}
	var refs []string
	for k := range obj {
		if strings.HasPrefix(k, "#") {
			refs = append(refs, k)
		}
	}
	if len(refs) == 0 {
		return args, nil
	}
	for _, k := range refs {
		var ref ResultReference
		if err := json.Unmarshal(obj[k], &ref); err != nil {
			return nil, &MethodError{Type: ErrInvalidResultReference,
				Description: fmt.Sprintf("%s is not a result reference: %v", k, err)}
		}
		name := strings.TrimPrefix(k, "#")
		if _, taken := obj[name]; taken {
			return nil, &MethodError{Type: ErrInvalidArguments,
				Description: fmt.Sprintf("%s is given both directly and as a back-reference", name)}
		}
		value, merr := lookupResult(ref, done)
		if merr != nil {
			return nil, merr
		}
		obj[name] = value
		delete(obj, k)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, &MethodError{Type: ErrServerFail, Description: err.Error()}
	}
	return out, nil
}

// lookupResult finds the referenced response and evaluates the path against it.
func lookupResult(ref ResultReference, done []Invocation) (json.RawMessage, *MethodError) {
	for _, inv := range done {
		if inv.CallID != ref.ResultOf {
			continue
		}
		// A call answered by "error" has no result to point into, and neither
		// has one that ran a different method than the client assumed.
		if inv.Name != ref.Name {
			return nil, &MethodError{Type: ErrInvalidResultReference,
				Description: fmt.Sprintf("call %q responded with %q, not %q", ref.ResultOf, inv.Name, ref.Name)}
		}
		v, err := evalPointer(inv.Args, ref.Path)
		if err != nil {
			return nil, &MethodError{Type: ErrInvalidResultReference, Description: err.Error()}
		}
		return v, nil
	}
	// Not found means it has not run yet, or never existed. Both are the same
	// error to the client: references only ever point backwards.
	return nil, &MethodError{Type: ErrInvalidResultReference,
		Description: fmt.Sprintf("no earlier call with id %q", ref.ResultOf)}
}

// evalPointer walks a JSON Pointer (RFC 6901) with JMAP's "*" extension: a "*"
// token maps the rest of the path over an array and flattens one level
// (RFC 8620 §3.7).
func evalPointer(doc json.RawMessage, path string) (json.RawMessage, error) {
	if path == "" {
		return doc, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path %q does not start with /", path)
	}
	return walk(doc, strings.Split(path[1:], "/"))
}

func walk(doc json.RawMessage, tokens []string) (json.RawMessage, error) {
	if len(tokens) == 0 {
		return doc, nil
	}
	token := unescapePointer(tokens[0])
	rest := tokens[1:]

	if token == "*" {
		var arr []json.RawMessage
		if err := json.Unmarshal(doc, &arr); err != nil {
			return nil, fmt.Errorf("* applied to a value that is not an array")
		}
		out := make([]json.RawMessage, 0, len(arr))
		for _, item := range arr {
			v, err := walk(item, rest)
			if err != nil {
				return nil, err
			}
			// Flatten one level: mapping over an array of arrays yields a
			// single array, which is what a client passes as "ids".
			var inner []json.RawMessage
			if json.Unmarshal(v, &inner) == nil {
				out = append(out, inner...)
				continue
			}
			out = append(out, v)
		}
		return json.Marshal(out)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err == nil {
		v, ok := obj[token]
		if !ok {
			return nil, fmt.Errorf("no member %q in the referenced result", token)
		}
		return walk(v, rest)
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(doc, &arr); err == nil {
		idx, err := strconv.Atoi(token)
		if err != nil {
			return nil, fmt.Errorf("%q is not an array index", token)
		}
		if idx < 0 || idx >= len(arr) {
			return nil, fmt.Errorf("index %d is out of range (%d items)", idx, len(arr))
		}
		return walk(arr[idx], rest)
	}
	return nil, fmt.Errorf("cannot follow %q into a value that is neither object nor array", token)
}

// unescapePointer applies RFC 6901 §3: ~1 is "/" and ~0 is "~", in that order,
// or "~01" would decode to "/" instead of "~1".
func unescapePointer(token string) string {
	token = strings.ReplaceAll(token, "~1", "/")
	return strings.ReplaceAll(token, "~0", "~")
}
