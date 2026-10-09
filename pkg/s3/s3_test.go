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
