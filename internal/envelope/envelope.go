// Package envelope reads and writes relay's envelope protocol. A call
// arrives on stdin as one JSON object:
//
//	{"input": {...}, "labels": ["private"], "call_id": "toolu_1"}
//
// and the tool answers with exactly one object on stdout:
//
//	{"content": "...", "is_error": false, "labels": ["contaminated"]}
//
// Parsing is strict. Every key must be one this knows, spelt exactly and
// given once, because two parsers that disagree about a repeated key or a
// key in another case would read one text two ways.
package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// MaxBytes bounds what is read from stdin.
const MaxBytes = 1 << 20

// Labels relay knows. A label outside this set makes relay reject the
// whole result.
var knownLabels = []string{"contaminated", "private"}

// Call is relay's request.
type Call struct {
	Input  map[string]json.RawMessage // the model's arguments, keys exact
	Labels []string                   // the calling session's labels
	CallID string
}

// Result is the reply to relay.
type Result struct {
	Content string   `json:"content"`
	IsError bool     `json:"is_error"`
	Labels  []string `json:"labels"`
}

// Read reads one call from r. The error never quotes the input.
func Read(r io.Reader) (Call, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return Call{}, errors.New("could not read the call")
	}
	return Parse(raw)
}

// Parse reads one call from raw.
func Parse(raw []byte) (Call, error) {
	if len(raw) > MaxBytes {
		return Call{}, fmt.Errorf("the call is larger than %d bytes", MaxBytes)
	}
	top, err := Object(raw, []string{"input", "labels", "call_id"}, nil)
	if err != nil {
		return Call{}, fmt.Errorf("bad envelope: %w", err)
	}
	var c Call
	if !isKind(top["labels"], '[') {
		return Call{}, errors.New("bad envelope: labels must be a list")
	}
	if err := json.Unmarshal(top["labels"], &c.Labels); err != nil {
		return Call{}, errors.New("bad envelope: labels must be a list of strings")
	}
	for _, l := range c.Labels {
		if !slices.Contains(knownLabels, l) {
			return Call{}, errors.New("bad envelope: labels must be among " + strings.Join(knownLabels, ", "))
		}
	}
	if c.CallID, err = String(top["call_id"]); err != nil {
		return Call{}, errors.New("bad envelope: call_id must be a string")
	}
	if !isKind(top["input"], '{') {
		return Call{}, errors.New("bad envelope: input must be an object")
	}
	if c.Input, err = Object(top["input"], nil, nil); err != nil {
		return Call{}, fmt.Errorf("bad input: %w", err)
	}
	return c, nil
}

// Write writes one result to w, as one line.
func Write(w io.Writer, r Result) error {
	if r.Labels == nil {
		r.Labels = []string{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return err
	}
	_, err := w.Write(b.Bytes())
	return err
}

// Object decodes one JSON object, and nothing after it, into its members.
// With required or optional given, a key outside them and a missing
// required key are errors; with both nil any key is accepted. A key given
// twice is always an error. Keys are compared exactly, unlike
// encoding/json, which matches struct fields without regard to case.
func Object(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	checkKeys := required != nil || optional != nil
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("not a JSON object")
		}
		key := tok.(string) // inside an object, a token here is a key
		if checkKeys && !slices.Contains(required, key) && !slices.Contains(optional, key) {
			return nil, errors.New("unknown key; the keys are " + strings.Join(append(slices.Clone(required), optional...), ", "))
		}
		if _, dup := out[key]; dup {
			return nil, errors.New("a key is given twice")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("not valid JSON")
		}
		if err := noDuplicates(v); err != nil {
			return nil, err
		}
		out[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, errors.New("not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("something follows the object")
	}
	for _, key := range required {
		if _, ok := out[key]; !ok {
			return nil, fmt.Errorf("%s is missing", key)
		}
	}
	return out, nil
}

// noDuplicates reports a repeated key anywhere inside a JSON value.
func noDuplicates(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return errors.New("not valid JSON")
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return errors.New("not valid JSON")
				}
				if seen[k.(string)] {
					return errors.New("a key is given twice")
				}
				seen[k.(string)] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		case json.Delim('['):
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		}
		return err
	}
	return walk()
}

// isKind reports whether a JSON value starts with the given delimiter.
func isKind(raw json.RawMessage, open byte) bool {
	v := bytes.TrimSpace(raw)
	return len(v) > 0 && v[0] == open
}

// String decodes a JSON string. Null and other kinds are errors.
func String(raw json.RawMessage) (string, error) {
	if !isKind(raw, '"') {
		return "", errors.New("not a string")
	}
	var s string
	err := json.Unmarshal(raw, &s)
	return s, err
}

// Int decodes a JSON integer, or a string holding one, since a model
// sometimes quotes a number.
func Int(raw json.RawMessage) (int, error) {
	v := bytes.TrimSpace(raw)
	if isKind(v, '"') {
		s, err := String(v)
		if err != nil {
			return 0, err
		}
		v = []byte(strings.TrimSpace(s))
	}
	var n int
	// Unmarshal leaves n alone for null, so null would read as 0.
	if string(v) == "null" {
		return 0, errors.New("not an integer")
	}
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, errors.New("not an integer")
	}
	return n, nil
}

// List decodes a JSON array into its elements.
func List(raw json.RawMessage) ([]json.RawMessage, error) {
	if !isKind(raw, '[') {
		return nil, errors.New("not a list")
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("not a list")
	}
	return out, nil
}
