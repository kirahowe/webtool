package envelope

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

func TestWriteIsOneExactResult(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := Result{
			Content: rapid.String().Draw(t, "content"),
			IsError: rapid.Bool().Draw(t, "is_error"),
			Labels:  rapid.SliceOfDistinct(rapid.SampledFrom(knownLabels), rapid.ID).Draw(t, "labels"),
		}
		var b bytes.Buffer
		if err := Write(&b, r); err != nil {
			t.Fatal(err)
		}
		if _, err := Object(b.Bytes(), []string{"content", "is_error", "labels"}, nil); err != nil {
			t.Fatalf("%v: %s", err, b.String())
		}
		var back Result
		if err := json.Unmarshal(b.Bytes(), &back); err != nil {
			t.Fatal(err)
		}
		// encoding/json replaces invalid UTF-8, so the content comes back
		// unchanged only when it was valid.
		if back.IsError != r.IsError || !slices.Equal(back.Labels, r.Labels) ||
			utf8.ValidString(r.Content) && back.Content != r.Content {
			t.Fatalf("%+v came back as %+v", r, back)
		}
	})
}

func TestNilLabelsWrittenAsList(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, Result{Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if b.String() != `{"content":"x","is_error":false,"labels":[]}`+"\n" {
		t.Fatalf("%q", b.String())
	}
}

func TestParse(t *testing.T) {
	c, err := Parse([]byte(`{"input": {"url": "u", "n": {"a": [1, {"b": 2}]}}, "labels": ["private"], "call_id": ""}`))
	if err != nil || string(c.Input["url"]) != `"u"` || c.Labels[0] != "private" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{
		`{"input": {"n": {"a": 1, "a": 2}}, "labels": [], "call_id": ""}`,
		`{"input": {"n": [{"a": 1, "a": 2}]}, "labels": [], "call_id": ""}`,
		`{"input": {}, "labels": [1], "call_id": ""}`,
		`{"input": {}, "labels": [], "call_id": null}`,
		`{"input": null, "labels": [], "call_id": ""}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestInt(t *testing.T) {
	for raw, want := range map[string]int{`3`: 3, `"4"`: 4, `" 5 "`: 5} {
		if n, err := Int(json.RawMessage(raw)); err != nil || n != want {
			t.Errorf("%s: %d %v", raw, n, err)
		}
	}
	for _, raw := range []string{`1.5`, `"x"`, `null`, `[]`, `1e2`} {
		if _, err := Int(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
