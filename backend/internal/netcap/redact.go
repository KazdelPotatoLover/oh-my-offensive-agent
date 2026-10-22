package netcap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

const previewLimit = 512

var sensitiveHeaderNames = map[string]struct{}{
	"authorization": {}, "cookie": {}, "set-cookie": {}, "x-api-key": {}, "proxy-authorization": {},
}

var sensitiveQueryKeys = map[string]struct{}{
	"token": {}, "password": {}, "secret": {}, "api_key": {}, "apikey": {}, "access_token": {},
}

func isSensitiveHeader(name string) bool {
	_, ok := sensitiveHeaderNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func redactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for key, values := range h {
		if isSensitiveHeader(key) {
			out[key] = []string{"[redacted]"}
			continue
		}
		copied := append([]string(nil), values...)
		out[key] = copied
	}
	return out
}

func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for key, values := range h {
		out[key] = strings.Join(values, ", ")
	}
	return out
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	q := u.Query()
	for key := range q {
		if _, ok := sensitiveQueryKeys[strings.ToLower(key)]; ok {
			q.Set(key, "[redacted]")
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

func redactBodyPreview(body []byte, binary bool) string {
	if binary || !utf8.Valid(body) {
		return "[binary]"
	}
	text := string(body)
	text = redactSensitiveText(text)
	if len(text) > previewLimit {
		return text[:previewLimit]
	}
	return text
}

func redactSensitiveText(text string) string {
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		redactJSON(parsed)
		encoded, err := json.Marshal(parsed)
		if err == nil {
			return string(encoded)
		}
	}
	return redactFormish(text)
}

func redactJSON(v any) {
	switch t := v.(type) {
	case map[string]any:
		for key, value := range t {
			if isSecretKey(key) {
				t[key] = "[redacted]"
				continue
			}
			redactJSON(value)
		}
	case []any:
		for _, item := range t {
			redactJSON(item)
		}
	}
}

func isSecretKey(key string) bool {
	switch strings.ToLower(key) {
	case "password", "passwd", "secret", "token", "access_token", "api_key", "authorization", "cookie":
		return true
	default:
		return false
	}
}

func redactFormish(text string) string {
	if !strings.Contains(text, "=") {
		return text
	}
	values, err := url.ParseQuery(text)
	if err != nil {
		return text
	}
	changed := false
	for key := range values {
		if isSecretKey(key) {
			values.Set(key, "[redacted]")
			changed = true
		}
	}
	if !changed {
		return text
	}
	return values.Encode()
}

func hashParts(status int, header http.Header, body []byte, method, rawURL string) string {
	h := sha256.New()
	if method != "" {
		h.Write([]byte(method))
		h.Write([]byte{'\n'})
		h.Write([]byte(rawURL))
		h.Write([]byte{'\n'})
	} else {
		h.Write([]byte(itoa(status)))
		h.Write([]byte{'\n'})
	}
	writeCanonicalHeaders(h, header)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func writeCanonicalHeaders(h interface{ Write([]byte) (int, error) }, header http.Header) {
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := append([]string(nil), header[key]...)
		sort.Strings(values)
		h.Write([]byte(strings.ToLower(key)))
		h.Write([]byte{':'})
		h.Write([]byte(strings.Join(values, ",")))
		h.Write([]byte{'\n'})
	}
}

func itoa(n int) string {
	return strings.TrimPrefix(strings.ReplaceAll(jsonNumber(n), ",", ""), "+")
}

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
