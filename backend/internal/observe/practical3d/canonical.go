package practical3d

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ExpectedManifestSchemaSHA256 is SHA-256 of the NFC/LF text of
// docs/frente3/06-benchmark-plan.md §§3.1–3.7 (from the "### 3.1 " heading up
// to, excluding, the "## 4." heading). TestEmbeddedManifestSchemaDigestMatchesNormativeDocs
// recomputes it from the checked-in document.
const ExpectedManifestSchemaSHA256 = "689b6f4bdd7cbcda11f120ad9a1f4b705bf4b4764f7eb07c3b8e17798dd826fa"

// ErrInvalidManifest marks every manifest rejection (PRESTART_INVALID).
var ErrInvalidManifest = errors.New("3d practical: invalid manifest")

var (
	canonicalInteger = regexp.MustCompile(`^(0|-?[1-9]\d*)$`)
	canonicalDecimal = regexp.MustCompile(`^-?(0|[1-9]\d*)\.\d*[1-9]$`)
)

// decimalPolicy reports whether a non-integer decimal is allowed at a JSON path.
type decimalPolicy func(path []string) bool

func noDecimals([]string) bool { return false }

// manifestDecimals allows decimals only for the four frozen §6 threshold
// constants; every other manifest number must be an integer (06 §3.1).
func manifestDecimals(path []string) bool { return len(path) == 2 && path[0] == "thresholds" }

func anyDecimals([]string) bool { return true }

func strictUnmarshal(raw []byte, dst any) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return requireEOF(dec)
}

// DecodeManifest decodes a closed-schema manifest. Besides unknown and
// duplicate keys, it rejects any input whose canonical form differs from the
// canonical form of the decoded struct: a missing required field, a null, or
// a prohibited empty field would otherwise decode silently to a zero value.
func DecodeManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if err := strictUnmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: decode closed schema: %w", ErrInvalidManifest, err)
	}
	fromRaw, err := canonicalRaw(raw, manifestDecimals)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: canonicalize input: %w", ErrInvalidManifest, err)
	}
	fromStruct, err := CanonicalManifest(m)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: canonicalize manifest: %w", ErrInvalidManifest, err)
	}
	if !bytes.Equal(fromRaw, fromStruct) {
		return Manifest{}, fmt.Errorf("%w: input has missing, null, prohibited, or non-canonical fields", ErrInvalidManifest)
	}
	if err := ValidateManifest(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// CanonicalManifest is canonical_bytes(manifest) of 06 §3.1.
func CanonicalManifest(m Manifest) ([]byte, error) { return canonicalWith(m, manifestDecimals) }

// ExperimentID is SHA-256 of canonical_bytes(manifest); it equals manifest_sha256.
func ExperimentID(m Manifest) (string, error) {
	b, err := CanonicalManifest(m)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// CanonicalJSON normalizes every string/key to NFC, accepts only canonical
// integers, sorts object keys by UTF-8 bytes, emits no insignificant
// whitespace, and disables HTML-only escaping.
func CanonicalJSON(v any) ([]byte, error) { return canonicalWith(v, noDecimals) }

// canonicalRequestJSON is the frozen provider-request serialization: the same
// rules as CanonicalJSON except that canonical decimals (no exponent, no
// trailing zeros) are allowed because provider payloads carry sampling values.
func canonicalRequestJSON(v any) ([]byte, error) { return canonicalWith(v, anyDecimals) }

func canonicalWith(v any, decimals decimalPolicy) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonicalRaw(raw, decimals)
}

func canonicalRaw(raw []byte, decimals decimalPolicy) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	if err := requireEOF(dec); err != nil {
		return nil, err
	}
	normalized, err := normalizeJSON(generic, nil, decimals)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func normalizeJSON(v any, path []string, decimals decimalPolicy) (any, error) {
	switch x := v.(type) {
	case nil, bool:
		return x, nil
	case string:
		return norm.NFC.String(x), nil
	case json.Number:
		s := x.String()
		if canonicalInteger.MatchString(s) {
			return x, nil
		}
		if decimals(path) && canonicalDecimal.MatchString(s) {
			return x, nil
		}
		return nil, fmt.Errorf("non-canonical or disallowed number %q at %s", s, strings.Join(path, "."))
	case []any:
		out := make([]any, len(x))
		for i := range x {
			var err error
			out[i], err = normalizeJSON(x[i], append(path, "[]"), decimals)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			nk := norm.NFC.String(k)
			if _, exists := out[nk]; exists {
				return nil, fmt.Errorf("duplicate object key after NFC normalization: %q", nk)
			}
			nv, err := normalizeJSON(value, append(path, nk), decimals)
			if err != nil {
				return nil, err
			}
			out[nk] = nv
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported canonical JSON value %s", reflect.TypeOf(v))
	}
}

func requireEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := scanJSONValue(dec); err != nil {
		return err
	}
	return requireEOF(dec)
}

func scanJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = true
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	return nil
}

func sha256Hex(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func validSHA256(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
