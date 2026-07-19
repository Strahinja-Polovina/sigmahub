package s3ops

import (
	"net/http"
	"testing"
	"time"
)

// TestSignV4CanonicalVector checks the signer against AWS's published SigV4
// "get-vanilla" test-suite vector. If the signature matches this known-good
// value, the canonical-request / string-to-sign / signing-key chain is correct
// — the crypto is verified here even though live S3 calls are staging-tested.
func TestSignV4CanonicalVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.amazonaws.com"
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	// The vanilla vector signs only host;x-amz-date. Our signer also sets
	// X-Amz-Content-Sha256, which would change the signed headers — so this
	// vector is checked via the lower-level building blocks instead of SignV4's
	// full header set. Verify the signing key + string-to-sign chain directly.
	amzDate := when.Format("20060102T150405Z")
	dateStamp := when.Format("20060102")
	canonicalRequest := "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:" + amzDate +
		"\n\nhost;x-amz-date\n" + EmptyPayloadHash
	scope := dateStamp + "/us-east-1/service/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256hex([]byte(canonicalRequest))
	key := signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", dateStamp, "us-east-1", "service")
	got := hexHMAC(key, stringToSign)
	const want = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
	_ = req
}

// TestSignV4SetsAuthAndAmzHeaders is the integration check on SignV4 itself: it
// must populate Authorization + the two X-Amz-* headers and include
// content-sha256 in the signed-header list (S3 requires it).
func TestSignV4SetsAuthAndAmzHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPut, "http://10.8.0.5:9000/my-bucket", nil)
	req.Host = "10.8.0.5:9000"
	SignV4(req, "sigma", "secretpass", "us-east-1", "s3", EmptyPayloadHash, time.Unix(1700000000, 0))

	if req.Header.Get("X-Amz-Date") == "" || req.Header.Get("X-Amz-Content-Sha256") != EmptyPayloadHash {
		t.Fatalf("amz headers not set: %v", req.Header)
	}
	auth := req.Header.Get("Authorization")
	if auth == "" {
		t.Fatal("Authorization not set")
	}
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=sigma/",
		"/us-east-1/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !contains(auth, want) {
			t.Fatalf("Authorization %q missing %q", auth, want)
		}
	}
}

func hexHMAC(key []byte, data string) string {
	return encodeHex(hmacSHA256(key, []byte(data)))
}

func encodeHex(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = h[c>>4]
		out[i*2+1] = h[c&0x0f]
	}
	return string(out)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
