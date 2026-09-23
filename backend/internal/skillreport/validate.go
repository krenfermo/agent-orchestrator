// Package skillreport decides whether a report produced by a skill AGENT may be
// stored, and what it looks like when it is (Frente 2 / 2C, ADR 0010).
//
// An agent's report is text a language model wrote after reading
// attacker-influenced source. Nothing in it is evidence until AO has checked
// it, so the order is fixed and every step is a refusal:
//
//  1. Validate the bytes against the package's declared JSON Schema, strictly:
//     duplicate keys, trailing data, unknown properties, a wrong type, an
//     out-of-range enum and an over-long string are all rejections.
//  2. Redact anything secret-shaped, and every literal credential AO itself
//     found in the staged files, from every string in the document.
//  3. Validate again, because redaction rewrote strings.
//
// Only then may a caller persist it. This package never persists anything.
package skillreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalidReport wraps every reason a document fails its schema.
var ErrInvalidReport = errors.New("skillreport: report does not validate")

// ErrUnsupportedSchema means the schema uses a keyword this validator does not
// implement. It is a refusal, not a pass: a validator that skipped a keyword it
// did not understand would accept exactly what that keyword exists to reject.
var ErrUnsupportedSchema = errors.New("skillreport: schema uses an unsupported keyword")

// MaxReportBytes bounds a report before it is parsed at all.
const MaxReportBytes = 1 << 20

// maxDepth bounds nesting, so a hostile document cannot exhaust the stack.
const maxDepth = 64

// supportedKeywords is the JSON Schema subset this validator implements. It is
// exactly what findings.v1.json uses, and the list is closed on purpose.
var supportedKeywords = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true,
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "const": true, "enum": true, "pattern": true, "format": true,
	"minLength": true, "maxLength": true, "minItems": true, "maxItems": true,
	"minimum": true, "maximum": true,
}

// Schema is a parsed schema, checked for unsupported keywords once.
type Schema struct {
	root map[string]any
}

// ParseSchema parses a JSON Schema document and refuses one that uses anything
// outside the supported subset.
func ParseSchema(raw []byte) (*Schema, error) {
	v, err := decodeStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("skillreport: parse schema: %w", err)
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("skillreport: schema must be an object")
	}
	if err := checkSchema(root, "#"); err != nil {
		return nil, err
	}
	return &Schema{root: root}, nil
}

func checkSchema(s map[string]any, at string) error {
	for k, v := range s {
		if !supportedKeywords[k] {
			return fmt.Errorf("%w: %q at %s", ErrUnsupportedSchema, k, at)
		}
		switch k {
		case "additionalProperties":
			// Only false is supported. true or a sub-schema would be a place
			// for arbitrary content, which is what strict validation removes.
			if b, ok := v.(bool); !ok || b {
				return fmt.Errorf("%w: additionalProperties must be false at %s", ErrUnsupportedSchema, at)
			}
		case "format":
			if v != "date-time" {
				return fmt.Errorf("%w: format %v at %s", ErrUnsupportedSchema, v, at)
			}
		case "pattern":
			p, ok := v.(string)
			if !ok {
				return fmt.Errorf("%w: pattern must be a string at %s", ErrUnsupportedSchema, at)
			}
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%w: pattern %q at %s: %w", ErrUnsupportedSchema, p, at, err)
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: properties must be an object at %s", ErrUnsupportedSchema, at)
			}
			for name, sub := range props {
				subMap, ok := sub.(map[string]any)
				if !ok {
					return fmt.Errorf("%w: property %q must be a schema at %s", ErrUnsupportedSchema, name, at)
				}
				if err := checkSchema(subMap, at+"/properties/"+name); err != nil {
					return err
				}
			}
		case "items":
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: items must be a single schema at %s", ErrUnsupportedSchema, at)
			}
			if err := checkSchema(sub, at+"/items"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks one report document against the schema.
func (s *Schema) Validate(doc []byte) error {
	if len(doc) > MaxReportBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", ErrInvalidReport, len(doc), MaxReportBytes)
	}
	v, err := decodeStrict(doc)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidReport, err)
	}
	return s.ValidateValue(v)
}

// ValidateValue checks an already-decoded document (as produced by Decode).
func (s *Schema) ValidateValue(v any) error {
	var errs []string
	validate(s.root, v, "$", 0, &errs)
	if len(errs) > 0 {
		sort.Strings(errs)
		if len(errs) > 20 {
			errs = append(errs[:20], fmt.Sprintf("... and %d more", len(errs)-20))
		}
		return fmt.Errorf("%w: %s", ErrInvalidReport, strings.Join(errs, "; "))
	}
	return nil
}

func validate(s map[string]any, v any, at string, depth int, errs *[]string) {
	if depth > maxDepth {
		*errs = append(*errs, at+": nested too deeply")
		return
	}
	fail := func(format string, args ...any) {
		*errs = append(*errs, at+": "+fmt.Sprintf(format, args...))
	}
	if t, ok := s["type"]; ok && !typeMatches(t, v) {
		fail("expected type %v, got %s", t, typeName(v))
		return
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, v) {
		fail("must equal %v", c)
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, want := range e {
			if jsonEqual(want, v) {
				found = true
				break
			}
		}
		if !found {
			fail("value is not one of the allowed values")
		}
	}
	switch val := v.(type) {
	case string:
		n := utf8.RuneCountInString(val)
		if m, ok := intKeyword(s, "maxLength"); ok && n > m {
			fail("string of %d characters exceeds maxLength %d", n, m)
		}
		if m, ok := intKeyword(s, "minLength"); ok && n < m {
			fail("string of %d characters is below minLength %d", n, m)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(val) {
			fail("does not match pattern %s", p)
		}
		if f, ok := s["format"].(string); ok && f == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, val); err != nil {
				fail("is not an RFC 3339 date-time")
			}
		}
	case json.Number:
		if m, ok := s["minimum"]; ok && compareNumber(val, m) < 0 {
			fail("is below minimum %v", m)
		}
		if m, ok := s["maximum"]; ok && compareNumber(val, m) > 0 {
			fail("is above maximum %v", m)
		}
	case []any:
		if m, ok := intKeyword(s, "minItems"); ok && len(val) < m {
			fail("has %d items, fewer than minItems %d", len(val), m)
		}
		if m, ok := intKeyword(s, "maxItems"); ok && len(val) > m {
			fail("has %d items, more than maxItems %d", len(val), m)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, item := range val {
				validate(items, item, fmt.Sprintf("%s[%d]", at, i), depth+1, errs)
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := val[name]; !present {
					fail("missing required property %q", name)
				}
			}
		}
		// additionalProperties is always false (checkSchema refuses anything
		// else), and an object schema that omits it is read the same way:
		// strictness is the default here, not something a schema opts into.
		if _, ok := s["additionalProperties"]; ok || props != nil {
			for name := range val {
				if _, declared := props[name]; !declared {
					fail("property %q is not allowed", name)
				}
			}
		}
		for name, sub := range props {
			child, present := val[name]
			if !present {
				continue
			}
			subMap, _ := sub.(map[string]any)
			validate(subMap, child, at+"."+name, depth+1, errs)
		}
	}
}

func typeMatches(t, v any) bool {
	switch tt := t.(type) {
	case string:
		return oneTypeMatches(tt, v)
	case []any:
		for _, one := range tt {
			if s, ok := one.(string); ok && oneTypeMatches(s, v) {
				return true
			}
		}
	}
	return false
}

func oneTypeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := strconv.ParseInt(n.String(), 10, 64)
		return err == nil
	}
	return false
}

func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func intKeyword(s map[string]any, key string) (int, bool) {
	n, ok := s[key].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(n.String())
	return i, err == nil
}

func compareNumber(v json.Number, bound any) int {
	b, ok := bound.(json.Number)
	if !ok {
		return 0
	}
	x, err1 := strconv.ParseFloat(v.String(), 64)
	y, err2 := strconv.ParseFloat(b.String(), 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

// Decode parses a document the way Validate does -- duplicate keys and
// trailing data rejected, numbers kept exact -- for a caller that needs to
// rewrite it (Redact) and validate the result.
func Decode(doc []byte) (any, error) {
	if len(doc) > MaxReportBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", ErrInvalidReport, len(doc), MaxReportBytes)
	}
	v, err := decodeStrict(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidReport, err)
	}
	return v, nil
}

// decodeStrict parses exactly one JSON value, rejecting duplicate object keys
// (encoding/json silently keeps the last, so a document could carry a valid
// value for the validator and a different one for a later reader) and any
// trailing content.
func decodeStrict(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the JSON document")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("document nested more than %d levels", maxDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				if _, dup := obj[key]; dup {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				val, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return t, nil
	}
}
