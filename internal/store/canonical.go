package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// CanonicalJSON normalises raw into the canonical JSON form:
//
//   - object keys sorted by Unicode code point, recursively
//   - no insignificant whitespace
//   - UTF-8, no BOM
//
// This is deliberately NOT RFC 8785 (JCS): number literals are passed through
// unchanged. "1.0" stays "1.0" and a 20-digit integer keeps all its digits,
// because the gateway hashes bodies it did not author and must not alter their
// meaning on the way to a hash.
//
// Two normalisations are unavoidable and therefore documented rather than
// hidden: string escapes are re-emitted in Go's encoding/json form (with HTML
// escaping off), and a duplicate object key keeps its last occurrence, which
// is encoding/json's rule.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("store: canonical json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("store: canonical json: trailing data after top-level value")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalValue serialises an in-memory value with the same rules. Values
// must be built from map[string]any, []any, string, bool, json.Number, int64,
// int, float64 or nil.
func CanonicalValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return writeJSONString(buf, t)
	case json.Number:
		// Validate before trusting the literal: json.Number is a string type
		// and CanonicalValue callers can construct an invalid one.
		if _, err := t.Float64(); err != nil {
			if _, ierr := t.Int64(); ierr != nil {
				return fmt.Errorf("store: canonical json: invalid number %q", string(t))
			}
		}
		buf.WriteString(string(t))
	case int:
		fmt.Fprintf(buf, "%d", t)
	case int64:
		fmt.Fprintf(buf, "%d", t)
	case float64:
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Go compares strings by byte; for well-formed UTF-8 that is the same
		// order as by Unicode code point.
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSONString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case json.RawMessage:
		nested, err := CanonicalJSON(t)
		if err != nil {
			return err
		}
		buf.Write(nested)
	default:
		return fmt.Errorf("store: canonical json: unsupported type %T", v)
	}
	return nil
}

func writeJSONString(buf *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}

// SHA256Hex returns the "sha256:<hex>" form used by content_sha256.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// BodySHA256 is the 32-byte digest stored in idempotency.body_sha256:
// SHA-256 over the canonical form of the request body.
func BodySHA256(rawBody []byte) ([]byte, error) {
	canon, err := CanonicalJSON(rawBody)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canon)
	return sum[:], nil
}

// contentSHA256 canonicalises the hash scope and returns "sha256:<hex>".
func contentSHA256(scope map[string]any) (string, error) {
	canon, err := CanonicalValue(scope)
	if err != nil {
		return "", err
	}
	return SHA256Hex(canon), nil
}
