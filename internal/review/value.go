package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Object is a JSON object that keeps its keys in the order the model wrote
// them. Models answer with whatever JSON they like ("summary" as an object,
// "confidence" as "high"), so a report is read as values and coerced field
// by field rather than decoded into a struct.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{vals: map[string]any{}} }

// Get returns the value at key, nil when absent.
func (o *Object) Get(key string) any {
	if o == nil {
		return nil
	}
	return o.vals[key]
}

// Has reports whether key is present.
func (o *Object) Has(key string) bool {
	if o == nil {
		return false
	}
	_, ok := o.vals[key]
	return ok
}

// Set adds or replaces key, keeping its position when it exists.
func (o *Object) Set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// MarshalJSON writes the object with its keys in order.
func (o *Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// parseValue decodes one JSON value: *Object, []any, string, json.Number,
// bool or nil. Trailing data after the value is an error.
func parseValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key %v is not a string", kt)
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			list := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil
	}
}

// asText is a report string field whatever JSON type the model used for it.
// Models answer `"summary": {"score": 3.6, "comments": [...]}` as often as a
// plain string: an object becomes "key: value; ...", a list its items joined
// with "; ", a number or boolean its text, null the empty string.
func asText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case *Object:
		var parts []string
		for _, k := range t.Keys() {
			if s := asText(t.Get(k)); s != "" {
				parts = append(parts, k+": "+s)
			}
		}
		return strings.Join(parts, "; ")
	case []any:
		var parts []string
		for _, e := range t {
			if s := asText(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return numberText(t)
	case float64:
		return floatText(t)
	case int:
		return strconv.Itoa(t)
	}
	return fmt.Sprint(v)
}

// numberText prints a JSON number the way the Python engine did: integers
// as written, floats in their shortest form with at least one decimal.
func numberText(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := n.Float64()
	if err != nil {
		return s
	}
	return floatText(f)
}

func floatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if a := math.Abs(f); a != 0 && (a < 1e-4 || a >= 1e16) {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign, digits := exp[:1], strings.TrimLeft(exp[1:], "0")
		if len(digits) < 2 {
			digits = strings.Repeat("0", 2-len(digits)) + digits
		}
		return mant + "e" + sign + digits
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// asInt is an integer field: a number (truncated), a string of digits or a
// boolean; anything else is 0.
func asInt(v any) int {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
		if f, err := t.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return int(f)
		}
	case string:
		if i, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(t), "_", "")); err == nil {
			return i
		}
	case bool:
		if t {
			return 1
		}
	case int:
		return t
	case float64:
		if !math.IsNaN(t) && !math.IsInf(t, 0) {
			return int(t)
		}
	}
	return 0
}

// asFloat is a number field, ok false when v holds no number.
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

// truthy is whether a value counts as given: not null, an empty string,
// zero, false or an empty list or object.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case *Object:
		return t != nil && len(t.keys) > 0
	}
	return true
}

// records is the objects in a list field, skipping anything else.
func records(o *Object, key string) []*Object {
	list, _ := o.Get(key).([]any)
	var out []*Object
	for _, e := range list {
		if r, ok := e.(*Object); ok {
			out = append(out, r)
		}
	}
	return out
}
