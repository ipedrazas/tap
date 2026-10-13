package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var creds = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

// The expected signature was produced by `aws s3api put-object` (aws-cli
// 2.32) for the same request, captured by a local server.
const awsSignature = "b47e7591d89c7df6ced7a731aab4c749d866d41fe4dd57ae2b0da9c719e9ca84"

func TestPutMatchesAWSCLI(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()

	c := &Client{
		Endpoint:    srv.URL,
		Region:      "auto",
		Bucket:      "tap-sessions",
		Credentials: func() (Credentials, error) { return creds, nil },
		HTTP:        srv.Client(),
		Now:         func() time.Time { return time.Date(2026, 10, 9, 12, 38, 5, 0, time.UTC) },
	}
	if err := c.Put(context.Background(), "echo/ab12 x.trace.json", "application/json", []byte("hello trace")); err != nil {
		t.Fatal(err)
	}
	if got.URL.EscapedPath() != "/tap-sessions/echo/ab12%20x.trace.json" || body != "hello trace" {
		t.Fatalf("PUT %s body %q", got.URL.EscapedPath(), body)
	}
	if h := got.Header.Get("X-Amz-Content-Sha256"); h != "130d7443dc1448e4785957103c2b943cfa644afae40a0357278d6089d4029bf5" {
		t.Errorf("payload hash %s", h)
	}

	// The AWS CLI signed for host 127.0.0.1:18999; re-sign that request.
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:18999/tap-sessions/echo/ab12%20x.trace.json", nil)
	req.Header.Set("Content-Type", "application/json")
	Sign(req, "130d7443dc1448e4785957103c2b943cfa644afae40a0357278d6089d4029bf5", "auto", creds, time.Date(2026, 10, 9, 12, 38, 5, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261009/auto/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=" + awsSignature
	if a := req.Header.Get("Authorization"); a != want {
		t.Errorf("authorization\n got %s\nwant %s", a, want)
	}
}

func TestPutError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Region: "auto", Bucket: "b", Credentials: func() (Credentials, error) { return creds, nil }, HTTP: srv.Client()}
	err := c.Put(context.Background(), "k", "text/plain", nil)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err %v", err)
	}
}

func TestListGetHead(t *testing.T) {
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
			t.Errorf("unsigned request %s", r.URL)
		}
		switch {
		case r.URL.Path == "/b" && r.URL.Query().Get("continuation-token") == "":
			if r.URL.Query().Get("prefix") != "echo/" || r.URL.Query().Get("list-type") != "2" {
				t.Errorf("list query %s", r.URL.RawQuery)
			}
			io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>n1</NextContinuationToken><Contents><Key>echo/a.trace.json</Key><Size>10</Size><LastModified>2026-10-13T10:00:00.000Z</LastModified></Contents></ListBucketResult>`)
		case r.URL.Path == "/b":
			io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>echo/b.trace.json</Key><Size>20</Size><LastModified>2026-10-13T11:00:00.000Z</LastModified></Contents></ListBucketResult>`)
		case r.URL.Path == "/b/echo/a.trace.json":
			ranges = append(ranges, r.Header.Get("Range"))
			io.WriteString(w, "0123456789")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Region: "auto", Bucket: "b", Credentials: func() (Credentials, error) { return creds, nil }, HTTP: srv.Client()}
	ctx := context.Background()

	objs, truncated, err := c.List(ctx, "echo/", 100)
	if err != nil || truncated || len(objs) != 2 || objs[1].Key != "echo/b.trace.json" || objs[1].Size != 20 || objs[1].LastModified.Hour() != 11 {
		t.Fatalf("List = %+v, %v, %v", objs, truncated, err)
	}
	if objs, truncated, _ := c.List(ctx, "echo/", 1); len(objs) != 1 || !truncated {
		t.Errorf("List max 1 = %d objects, truncated %v", len(objs), truncated)
	}
	if b, err := c.Get(ctx, "echo/a.trace.json", 100); err != nil || string(b) != "0123456789" {
		t.Errorf("Get = %q, %v", b, err)
	}
	if _, err := c.Get(ctx, "echo/a.trace.json", 5); err == nil {
		t.Error("Get over max: no error")
	}
	// A server that ignores Range still yields only the first n bytes.
	if b, err := c.Head(ctx, "echo/a.trace.json", 4); err != nil || string(b) != "0123" {
		t.Errorf("Head = %q, %v", b, err)
	}
	if ranges[len(ranges)-1] != "bytes=0-3" {
		t.Errorf("Range = %q", ranges[len(ranges)-1])
	}
	if _, err := c.Get(ctx, "echo/missing.trace.json", 100); err != ErrNotFound {
		t.Errorf("missing object: %v", err)
	}
}
