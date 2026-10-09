// Package s3 writes objects to an S3-compatible store (Tigris) with AWS
// Signature Version 4. The harness only ever PUTs whole objects, so this is a
// signer and one request, not an SDK.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type Credentials struct {
	AccessKeyID, SecretAccessKey string
}

// Client puts objects into one bucket, addressed path-style
// (<endpoint>/<bucket>/<key>) so the only host it reaches is the endpoint.
type Client struct {
	Endpoint string // https://t3.storage.dev
	Region   string // "auto" for Tigris
	Bucket   string
	// Credentials is called for every request, so rotated keys are picked up.
	Credentials func() (Credentials, error)
	HTTP        *http.Client
	Now         func() time.Time
}

// Put uploads body as key, replacing any existing object.
func (c *Client) Put(ctx context.Context, key, contentType string, body []byte) error {
	creds, err := c.Credentials()
	if err != nil {
		return fmt.Errorf("s3 credentials: %w", err)
	}
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil {
		return err
	}
	u.Path += "/" + c.Bucket + "/" + key
	u.RawPath = encode(u.Path, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	Sign(req, hashHex(body), c.Region, creds, now())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("s3 put %s: %s: %s", key, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Sign adds SigV4 headers for service s3. It signs Host and every header the
// request already carries, plus x-amz-date and x-amz-content-sha256.
func Sign(req *http.Request, payloadHash, region string, creds Credentials, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range headers {
			if !yield(k) {
				return
			}
		}
	})
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		encode(or(req.URL.Path, "/"), true),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonical))

	k := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", creds.AccessKeyID, scope, signed, sig))
}

// encode URI-encodes s the way SigV4 canonicalises it: unreserved
// characters stay, everything else is %XX, and '/' stays only in paths.
func encode(s string, path bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~', path && ch == '/':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, encode(k, false)+"="+encode(v, false))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "&")
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
