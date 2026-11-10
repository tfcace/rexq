package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// object is a JSON object that keeps its keys in the order they were
// read, so a settings file is written back with nothing moved.
//
// The other JSON values are []any, string, json.Number (written back as
// it was read), bool and nil.
type object struct {
	keys []string
	vals map[string]any
}

// obj builds an object from key, value pairs.
func obj(pairs ...any) *object {
	o := &object{vals: map[string]any{}}
	for i := 0; i < len(pairs); i += 2 {
		o.set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func (o *object) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// set keeps the place of a key that is already there.
func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// parseJSON reads one JSON value.
func parseJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("it is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err == io.EOF {
		return nil, errors.New("it ends early")
	}
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("text follows the JSON value")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch d {
	case '{':
		o := obj()
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", k)
			}
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			o.set(key, v)
		}
		_, err := dec.Token()
		return o, err
	case '[':
		a := []any{}
		for dec.More() {
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	}
	return nil, fmt.Errorf("unexpected %v", d)
}

// encodeJSON writes v indented by two spaces, with a final newline, the
// way Claude Code's settings usually are. Text outside ASCII stays as it
// is, not \u-escaped.
func encodeJSON(v any) []byte {
	var b bytes.Buffer
	writeValue(&b, v, "")
	b.WriteByte('\n')
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any, indent string) {
	inner := indent + "  "
	switch v := v.(type) {
	case *object:
		if len(v.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range v.keys {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(inner)
			writeString(b, k)
			b.WriteString(": ")
			writeValue(b, v.vals[k], inner)
		}
		b.WriteString("\n" + indent + "}")
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range v {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(inner)
			writeValue(b, e, inner)
		}
		b.WriteString("\n" + indent + "]")
	case string:
		writeString(b, v)
	case json.Number:
		b.WriteString(v.String())
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		panic(fmt.Sprintf("install: no JSON for %T", v))
	}
}

var jsonEscapes = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\b", `\b`, "\f", `\f`)

// writeString quotes s, escaping only what JSON requires.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range jsonEscapes.Replace(s) {
		if r < 0x20 {
			fmt.Fprintf(b, `\u%04x`, r)
		} else {
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
