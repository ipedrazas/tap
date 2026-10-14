// casa-collector uploads the session files agents write to a shared
// directory into an S3-compatible bucket (Tigris). It runs beside the agent
// (a native sidecar in Kubernetes, a service in compose) so the agent holds
// no bucket credentials and carries no upload code.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "casa-collector:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "", "directory producers write session files to")
	endpoint := flag.String("endpoint", "https://t3.storage.dev", "S3 endpoint")
	bucket := flag.String("bucket", "", "bucket to upload to")
	region := flag.String("region", "auto", "region for request signing")
	prefix := flag.String("prefix", "", "key prefix prepended to each file's path under --dir")
	match := flag.String("match", "*.trace.json", "glob on file base names to upload")
	credsDir := flag.String("credentials-dir", "", "directory holding access-key-id and secret-access-key (re-read per request); empty uses AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	interval := flag.Duration("interval", 5*time.Second, "how often to scan --dir")
	settle := flag.Duration("settle", 2*time.Second, "skip files modified more recently than this (until shutdown)")
	maxSize := flag.Int64("max-size", 64<<20, "largest file uploaded, in bytes")
	stateFile := flag.String("state-file", "", "file remembering uploads across restarts (empty: re-upload everything on start)")
	drain := flag.Duration("drain-timeout", 20*time.Second, "time allowed for the final sweep on SIGTERM")
	once := flag.Bool("once", false, "sweep once, ignoring --settle, and exit")
	egressAddr := flag.String("egress-proxy", "", "tap-style egress proxy host:port (empty: direct, or HTTPS_PROXY)")
	egressKey := flag.String("egress-key-file", "", "HMAC key for egress proxy credentials")
	egressAgent := flag.String("egress-agent", "", "agent name the egress credentials are for")
	egressScope := flag.String("egress-scope", "", "egress scope the credentials are for")
	flag.Parse()
	if *dir == "" || *bucket == "" {
		return fmt.Errorf("--dir and --bucket are required")
	}
	if _, err := filepath.Match(*match, ""); err != nil {
		return fmt.Errorf("--match: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "casa-collector")
	// The default transport honours HTTPS_PROXY.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if *egressAddr != "" {
		if *egressKey == "" || *egressAgent == "" || *egressScope == "" {
			return fmt.Errorf("--egress-proxy needs --egress-key-file, --egress-agent and --egress-scope")
		}
		if _, err := readTrim(*egressKey); err != nil {
			return err
		}
		transport.Proxy = egressProxy(*egressAddr, *egressAgent, *egressScope, *egressKey)
	}
	c := &Collector{
		Dir:       *dir,
		Prefix:    *prefix,
		Match:     *match,
		Settle:    *settle,
		MaxSize:   *maxSize,
		StateFile: *stateFile,
		Logger:    logger,
		Store: &Bucket{
			Endpoint:    *endpoint,
			Region:      *region,
			Name:        *bucket,
			Credentials: credentials(*credsDir),
			HTTP:        &http.Client{Timeout: time.Minute, Transport: transport},
		},
	}
	if err := c.Load(); err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	if *once {
		n, err := c.Sweep(context.Background(), true)
		logger.Info("swept", "uploaded", n)
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	logger.Info("collecting", "dir", *dir, "endpoint", *endpoint, "bucket", *bucket, "prefix", *prefix, "match", *match)
	c.Run(ctx, *interval, *drain)
	return nil
}

func credentials(dir string) func() (Credentials, error) {
	if dir == "" {
		return func() (Credentials, error) {
			c := Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
			if c.AccessKeyID == "" || c.SecretAccessKey == "" {
				return c, fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are not set")
			}
			return c, nil
		}
	}
	return func() (Credentials, error) {
		id, err := readTrim(filepath.Join(dir, "access-key-id"))
		if err != nil {
			return Credentials{}, err
		}
		secret, err := readTrim(filepath.Join(dir, "secret-access-key"))
		return Credentials{AccessKeyID: id, SecretAccessKey: secret}, err
	}
}

func readTrim(p string) (string, error) {
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err
}
