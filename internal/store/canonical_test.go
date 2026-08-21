package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCanonicalJSONGolden pins the canonical JSON form as literal bytes.
//
// These strings are the contract between the gateway's body_sha256 and every
// future build of it: if any of them changes, every idempotency key already in
// a database stops matching and replay silently becomes a fresh dispatch. The
// number cases matter most — the canonical form is explicitly NOT RFC 8785, so 1.0 must stay
// 1.0 and a 20-digit integer must keep all 20 digits.
func TestCanonicalJSONGolden(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"key order", `{"b":1,"a":2,"c":3}`, `{"a":2,"b":1,"c":3}`},
		{"key order is by code point, not case-insensitive", `{"a":1,"B":2,"b":3,"A":4}`, `{"A":4,"B":2,"a":1,"b":3}`},
		{"whitespace stripped", "{\n  \"a\" : [ 1 , 2 ]\n}", `{"a":[1,2]}`},
		{"nesting is recursive", `{"z":{"y":1,"x":{"w":2,"v":3}},"a":0}`, `{"a":0,"z":{"x":{"v":3,"w":2},"y":1}}`},
		{"array order is preserved", `{"a":[3,1,2]}`, `{"a":[3,1,2]}`},
		{"1.0 is not 1", `{"a":1.0}`, `{"a":1.0}`},
		{"integer 1 stays 1", `{"a":1}`, `{"a":1}`},
		{"exponent form is preserved", `{"a":1e3}`, `{"a":1e3}`},
		{"trailing zeros preserved", `{"a":1.500}`, `{"a":1.500}`},
		{"large integer keeps all digits", `{"a":12345678901234567890}`, `{"a":12345678901234567890}`},
		{"int64 overflow keeps all digits", `{"a":99999999999999999999999999}`, `{"a":99999999999999999999999999}`},
		{"negative zero preserved", `{"a":-0.0}`, `{"a":-0.0}`},
		{"non-ASCII keys sort by code point", `{"é":1,"z":2,"a":3}`, `{"a":3,"z":2,"é":1}`},
		{"non-ASCII values pass through unescaped", `{"a":"ñïcodé ✓"}`, `{"a":"ñïcodé ✓"}`},
		{"HTML escaping is off", `{"a":"<b>&</b>"}`, `{"a":"<b>&</b>"}`},
		// encoding/json escapes U+2028/U+2029 unconditionally, even with
		// SetEscapeHTML(false). That is part of the "escapes are re-emitted in
		// Go form" normalisation the package documents; pinned here so a
		// future switch of encoder cannot change body_sha256 unnoticed.
		{"line separator U+2028 is escaped", "{\"a\":\"x y\"}", `{"a":"x\u2028y"}`},
		{"paragraph separator U+2029 is escaped", "{\"a\":\"x y\"}", `{"a":"x\u2029y"}`},
		{"duplicate key keeps the last occurrence", `{"a":1,"a":2}`, `{"a":2}`},
		{"duplicate key, last wins even out of order", `{"b":1,"a":9,"b":2}`, `{"a":9,"b":2}`},
		{"empty object", `{}`, `{}`},
		{"empty array", `[]`, `[]`},
		{"null", `null`, `null`},
		{"bare true", `true`, `true`},
		{"bare string", `"x"`, `"x"`},
		{"top-level array of objects", `[{"b":1,"a":2}]`, `[{"a":2,"b":1}]`},
		{"escapes are re-emitted in Go form", `{"a":"tab\there"}`, `{"a":"tab\there"}`},
		{"unicode escape is decoded", `{"a":"é"}`, `{"a":"é"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("CanonicalJSON(%s): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("CanonicalJSON(%s)\n got %s\nwant %s", tc.in, got, tc.want)
			}
			// Idempotent: canonicalising a canonical form changes nothing.
			again, err := CanonicalJSON(got)
			if err != nil {
				t.Fatalf("re-canonicalising %s: %v", got, err)
			}
			if string(again) != string(got) {
				t.Fatalf("CanonicalJSON is not idempotent: %s -> %s", got, again)
			}
		})
	}
}

// TestCanonicalJSONKeyOrderIsInputIndependent: the same object written in any
// key order must produce identical bytes. This is what makes body_sha256
// stable across callers that use different JSON libraries.
func TestCanonicalJSONKeyOrderIsInputIndependent(t *testing.T) {
	variants := []string{
		`{"alpha":1,"beta":{"x":true,"y":null},"gamma":[1,2]}`,
		`{"gamma":[1,2],"alpha":1,"beta":{"y":null,"x":true}}`,
		"{ \"beta\" : { \"x\" : true , \"y\" : null } , \"gamma\" : [ 1 , 2 ] , \"alpha\" : 1 }",
	}
	var want string
	for i, v := range variants {
		got, err := CanonicalJSON([]byte(v))
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if i == 0 {
			want = string(got)
			continue
		}
		if string(got) != want {
			t.Errorf("variant %d canonicalised to %s, want %s", i, got, want)
		}
	}
	if h1, err := BodySHA256([]byte(variants[0])); err == nil {
		h2, err := BodySHA256([]byte(variants[1]))
		if err != nil {
			t.Fatalf("BodySHA256: %v", err)
		}
		if string(h1) != string(h2) {
			t.Errorf("body_sha256 differs across key orderings of the same object")
		}
		if len(h1) != 32 {
			t.Errorf("body_sha256 is %d bytes, want 32", len(h1))
		}
	} else {
		t.Fatalf("BodySHA256: %v", err)
	}
}

// TestCanonicalJSONRejectsMalformed: the gateway hashes what a caller sent, so
// it must refuse anything it cannot parse rather than hash a partial value.
func TestCanonicalJSONRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		``,
		`{`,
		`{"a":}`,
		`{"a":1} {"b":2}`,       // trailing data
		`{"a":1}garbage`,        // trailing data
		"\xef\xbb\xbf{\"a\":1}", // BOM: the canonical form is UTF-8, no BOM
	} {
		if out, err := CanonicalJSON([]byte(in)); err == nil {
			t.Errorf("CanonicalJSON(%q) = %s, want an error", in, out)
		}
	}
}

// TestCanonicalValueTypes covers the in-memory path used by content_sha256.
func TestCanonicalValueTypes(t *testing.T) {
	got, err := CanonicalValue(map[string]any{
		"s": "x", "b": true, "n": nil, "i": 7, "i64": int64(-9),
		"f": 1.5, "num": json.Number("12345678901234567890"),
		"arr": []any{1, "two", nil},
		"raw": json.RawMessage(`{"z":1,"a":2}`),
	})
	if err != nil {
		t.Fatalf("CanonicalValue: %v", err)
	}
	want := `{"arr":[1,"two",null],"b":true,"f":1.5,"i":7,"i64":-9,"n":null,"num":12345678901234567890,"raw":{"a":2,"z":1},"s":"x"}`
	if string(got) != want {
		t.Fatalf("CanonicalValue\n got %s\nwant %s", got, want)
	}

	if _, err := CanonicalValue(map[string]any{"bad": json.Number("not-a-number")}); err == nil {
		t.Errorf("CanonicalValue accepted an invalid json.Number")
	}
	if _, err := CanonicalValue(map[string]any{"bad": struct{ A int }{1}}); err == nil {
		t.Errorf("CanonicalValue accepted an unsupported type")
	}
}

// TestSHA256HexForm pins the "sha256:<hex>" shape content_sha256 uses.
func TestSHA256HexForm(t *testing.T) {
	got := SHA256Hex([]byte("abc"))
	const want = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("SHA256Hex(abc) = %s, want %s", got, want)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("missing sha256: prefix")
	}
}
