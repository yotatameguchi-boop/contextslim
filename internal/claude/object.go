package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Object is a JSON object that keeps its key order and raw values, so files
// ContextSlim edits keep everything it does not own exactly as written.
type Object struct {
	keys []string
	vals map[string]json.RawMessage
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{vals: map[string]json.RawMessage{}} }

// ParseObject decodes a JSON object preserving key order.
func ParseObject(b []byte) (*Object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	o := NewObject()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := kt.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after object")
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data after object")
	}
	return o, nil
}

// Get returns the raw value for k.
func (o *Object) Get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// Set sets k, appending it if new.
func (o *Object) Set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Delete removes k.
func (o *Object) Delete(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len returns the number of keys.
func (o *Object) Len() int { return len(o.keys) }

// MarshalJSON writes the object in key order.
func (o *Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		v := o.vals[k]
		if !json.Valid(v) {
			return nil, fmt.Errorf("invalid value for %q", k)
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
