// Package s3 talks to an S3-compatible store (Tigris) with AWS Signature
// Version 4. The harness PUTs whole objects; the console lists and reads
// them. It is a signer and three requests, not an SDK.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is a missing object.
var ErrNotFound = errors.New("s3: no such object")

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
	resp, err := c.do(ctx, http.MethodPut, "/"+key, nil, map[string]string{"Content-Type": contentType}, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return failure("put "+key, resp)
	}
	return nil
}

// Object is one entry of a listing.
type Object struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
}

// List returns up to max objects under prefix (ListObjectsV2), following
// continuation tokens. truncated reports that more objects exist.
func (c *Client) List(ctx context.Context, prefix string, max int) (objs []Object, truncated bool, err error) {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, false, err
		}
		var page struct {
			Contents    []Object `xml:"Contents"`
			IsTruncated bool     `xml:"IsTruncated"`
			NextToken   string   `xml:"NextContinuationToken"`
		}
		if resp.StatusCode/100 != 2 {
			err = failure("list "+prefix, resp)
		} else {
			err = xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		}
		resp.Body.Close()
		if err != nil {
			return nil, false, err
		}
		objs = append(objs, page.Contents...)
		if !page.IsTruncated || page.NextToken == "" {
			return objs, false, nil
		}
		if len(objs) >= max {
			return objs[:max], true, nil
		}
		token = page.NextToken
	}
}

// Get reads a whole object, refusing one larger than max bytes.
func (c *Client) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	return c.get(ctx, key, nil, max, false)
}

// Head reads at most the first n bytes of an object (a Range request).
func (c *Client) Head(ctx context.Context, key string, n int64) ([]byte, error) {
	return c.get(ctx, key, map[string]string{"Range": "bytes=0-" + strconv.FormatInt(n-1, 10)}, n, true)
}

func (c *Client) get(ctx context.Context, key string, headers map[string]string, max int64, partial bool) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, "/"+key, nil, headers, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode/100 != 2:
		return nil, failure("get "+key, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		if !partial {
			return nil, fmt.Errorf("s3 get %s: larger than %d bytes", key, max)
		}
		b = b[:max]
	}
	return b, nil
}

// do sends one signed request for path (under the bucket) with query.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, headers map[string]string, body []byte) (*http.Response, error) {
	creds, err := c.Credentials()
	if err != nil {
		return nil, fmt.Errorf("s3 credentials: %w", err)
	}
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	u.Path += "/" + c.Bucket + path
	u.RawPath = encode(u.Path, true)
	if query != nil {
		u.RawQuery = canonicalQuery(query)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	Sign(req, hashHex(body), c.Region, creds, now())
	return c.HTTP.Do(req)
}

func failure(what string, resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return fmt.Errorf("s3 %s: %s: %s", what, resp.Status, strings.TrimSpace(string(msg)))
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
