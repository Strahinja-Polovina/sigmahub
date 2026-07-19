package s3ops

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// s3Region is the region label SigV4 signs with. The engines don't enforce a
// region, but the signature must agree with itself, so a fixed label is fine.
const s3Region = "us-east-1"

// S3Client issues signed S3-API requests to one resource's container over the
// mesh. Bucket create/delete/list and usage measurement are pure S3 API, so
// this path is identical for MinIO and SeaweedFS.
type S3Client struct {
	http      *http.Client
	endpoint  string // http://<meshIP>:<port>
	accessKey string
	secretKey string
	now       func() time.Time
}

// NewS3Client builds a client for one endpoint + root credential.
func NewS3Client(hc *http.Client, endpoint, accessKey, secretKey string) *S3Client {
	return &S3Client{http: hc, endpoint: strings.TrimSuffix(endpoint, "/"), accessKey: accessKey, secretKey: secretKey, now: time.Now}
}

func (c *S3Client) do(ctx context.Context, method, path, rawQuery string, body []byte) (*http.Response, error) {
	u := c.endpoint + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	payloadHash := EmptyPayloadHash
	if body != nil {
		payloadHash = sha256hex(body)
	}
	SignV4(req, c.accessKey, c.secretKey, s3Region, "s3", payloadHash, c.now())
	return c.http.Do(req)
}

// s3Error extracts the S3 error body (best effort) for diagnostics.
func s3Error(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("s3 %d: %s", resp.StatusCode, msg)
}

// CreateBucket issues PUT /<bucket>. A bucket that already exists (owned by us)
// is treated as success so the op is idempotent on resync.
func (c *S3Client) CreateBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodPut, "/"+bucket, "", []byte{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusConflict:
		// BucketAlreadyOwnedByYou / BucketAlreadyExists — idempotent create.
		return nil
	default:
		return s3Error(resp)
	}
}

// DeleteBucket issues DELETE /<bucket>. A missing bucket is idempotent success.
func (c *S3Client) DeleteBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/"+bucket, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return s3Error(resp)
}

type listBucketsResult struct {
	Buckets struct {
		Bucket []struct {
			Name string `xml:"Name"`
		} `xml:"Bucket"`
	} `xml:"Buckets"`
}

// ListBuckets issues GET / and returns the bucket names the root credential sees.
func (c *S3Client) ListBuckets(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, s3Error(resp)
	}
	var out listBucketsResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list-buckets: %w", err)
	}
	names := make([]string, 0, len(out.Buckets.Bucket))
	for _, b := range out.Buckets.Bucket {
		names = append(names, b.Name)
	}
	return names, nil
}

type listObjectsV2Result struct {
	Contents []struct {
		Size int64 `xml:"Size"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// MeasureBucket sums the sizes of every object in a bucket via paginated
// ListObjectsV2 — the engine-agnostic storage-bytes measurement fed to metering.
func (c *S3Client) MeasureBucket(ctx context.Context, bucket string) (int64, error) {
	var total int64
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "/"+bucket, canonicalQuery(q), nil)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err := s3Error(resp)
			resp.Body.Close()
			return 0, err
		}
		var page listObjectsV2Result
		derr := xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if derr != nil {
			return 0, fmt.Errorf("decode list-objects: %w", derr)
		}
		for _, o := range page.Contents {
			total += o.Size
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	return total, nil
}
