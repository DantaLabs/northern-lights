package assurance

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

func canonicalizeSetStrings(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			return nil, fmt.Errorf("set member must not be empty")
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("set member %q is duplicated", value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

// Digest is a fixed one-way SHA-256 digest. Using a distinct type prevents raw
// idempotency keys from accidentally reaching persistence APIs.
type Digest [sha256.Size]byte

// HashBytes returns the SHA-256 digest of value.
func HashBytes(value []byte) Digest { return sha256.Sum256(value) }

// DigestIdempotencyKey hashes the exact received key bytes.
func DigestIdempotencyKey(value string) Digest { return HashBytes([]byte(value)) }

// CanonicalJSON serializes v as UTF-8 JSON with lexicographically sorted object
// keys and source-order arrays.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical JSON: %w", err)
	}
	return CanonicalJSONBytes(raw)
}

// CanonicalJSONBytes validates one JSON value and returns canonical encoding.
func CanonicalJSONBytes(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode canonical JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("decode canonical JSON: multiple values")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode canonical JSON trailing content: %w", err)
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonical(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case string:
		encoded, _ := json.Marshal(typed)
		out.Write(encoded)
	case json.Number:
		if _, err := canonicalJSONNumber(typed.String()); err != nil {
			return err
		}
		out.WriteString(typed.String())
	case []any:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			if err := writeCanonical(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON type %T", value)
	}
	return nil
}

func canonicalJSONNumber(value string) (string, error) {
	// JSON number syntax permits exponents. Preserve the decoded number bytes;
	// domain decimals use canonicalDecimal and therefore reject exponents.
	number := json.Number(value)
	if _, err := number.Float64(); err != nil {
		return "", fmt.Errorf("invalid JSON number %q", value)
	}
	return value, nil
}
