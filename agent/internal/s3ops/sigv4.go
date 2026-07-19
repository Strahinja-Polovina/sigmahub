// Package s3ops implements the P2-1b (SIGMA-65) s3.configure agent op: bucket
// CRUD, per-bucket access keys, quotas, and usage measurement against a
// resource's own S3 container over the mesh. Bucket/object operations use the
// engine-agnostic S3 API (AWS SigV4, signed here with the stdlib — the agent
// carries no S3 SDK); engine-specific admin (per-bucket keys, quotas) dispatches
// to MinIO's Admin API or SeaweedFS's `weed shell` per engine.
//
// The signing below is exercised against the canonical AWS SigV4 test vector in
// sigv4_test.go, so the crypto is provably correct even though the live calls
// against a real MinIO/SeaweedFS are validated on staging.
package s3ops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// EmptyPayloadHash is sha256("") — the payload hash for a request with no body.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// signingKey derives the SigV4 signing key for a date/region/service.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// SignV4 signs req in place with AWS Signature Version 4: it sets X-Amz-Date and
// X-Amz-Content-Sha256, then the Authorization header. Every X-Amz-* header
// present plus Host (and Content-Type if set) is signed, so a caller that sets
// X-Amz-Content-Sha256 (as S3 requires) gets it folded into the signature.
// payloadHash is HexEncode(SHA256(body)); pass EmptyPayloadHash for no body.
func SignV4(req *http.Request, accessKey, secretKey, region, service, payloadHash string, t time.Time) {
	amzDate := t.UTC().Format("20060102T150405Z")
	dateStamp := t.UTC().Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Collect the headers to sign: host + Content-Type + all X-Amz-*.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	type kv struct{ name, value string }
	signed := []kv{{"host", host}}
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			signed = append(signed, kv{lower, strings.TrimSpace(strings.Join(vals, ","))})
		}
	}
	sort.Slice(signed, func(i, j int) bool { return signed[i].name < signed[j].name })

	var canonHeaders strings.Builder
	names := make([]string, 0, len(signed))
	for _, h := range signed {
		canonHeaders.WriteString(h.name)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(h.value)
		canonHeaders.WriteByte('\n')
		names = append(names, h.name)
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256hex([]byte(canonicalRequest)),
	}, "\n")

	sig := hex.EncodeToString(hmacSHA256(signingKey(secretKey, dateStamp, region, service), []byte(stringToSign)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// canonicalURI returns the already-escaped path, defaulting to "/". S3 path-style
// requests keep the path single-encoded, which url.EscapedPath already provides.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery renders the canonical query string: keys sorted, each
// key and value URI-encoded, joined by '&'.
func canonicalQuery(q map[string][]string) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is the AWS-flavored RFC 3986 encoding: unreserved chars pass through,
// everything else is %XX. When encodeSlash is false, '/' is left as-is (used for
// object keys in the path); AWS encodes '/' in query values (encodeSlash true).
func uriEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}
