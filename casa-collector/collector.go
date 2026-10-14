package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Putter stores one object (*Bucket in production).
type Putter interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
}

// Collector uploads the files producers write under Dir. A file is uploaded
// whenever its size or mtime changes, under Prefix + its path relative to
// Dir, so <dir>/<agent>/<session>.trace.json lands at
// <prefix>/<agent>/<session>.trace.json. Remote objects are never deleted.
type Collector struct {
	Dir    string
	Prefix string
	// Match is a glob on the base name ("*.trace.json").
	Match string
	// Settle skips files modified more recently than this, so a producer
	// that writes in place is not caught halfway. The final sweep ignores it.
	Settle  time.Duration
	MaxSize int64
	// StateFile, when set, keeps what was uploaded across restarts.
	StateFile string
	Store     Putter
	Logger    *slog.Logger
	Now       func() time.Time

	seen  map[string]stamp
	dirty bool
}

// stamp is what a file looked like when it was last uploaded (or skipped
// for good, when too large).
type stamp struct {
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime"`
}

// Load reads the state file, if any.
func (c *Collector) Load() error {
	c.seen = map[string]stamp{}
	if c.StateFile == "" {
		return nil
	}
	b, err := os.ReadFile(c.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &c.seen)
}

// Sweep uploads every matching file that changed since its last upload.
// Failed uploads keep their old stamp, so the next sweep retries them.
func (c *Collector) Sweep(ctx context.Context, final bool) (int, error) {
	if c.seen == nil {
		c.seen = map[string]stamp{}
	}
	// Opened through a Root, a symlink cannot reach outside Dir: a producer
	// must never be able to make the collector upload its credentials.
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	present := map[string]bool{}
	uploaded := 0
	var errs []error
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		// Producers write to a dotfile or *.tmp and rename it into place.
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		if ok, _ := filepath.Match(c.Match, name); !ok {
			return nil
		}
		present[rel] = true
		info, err := d.Info()
		if err != nil {
			return nil // renamed away since the listing
		}
		st := stamp{Size: info.Size(), MTime: info.ModTime().UnixNano()}
		if c.seen[rel] == st || (!final && now().Sub(info.ModTime()) < c.Settle) {
			return nil
		}
		st, err = c.upload(ctx, root, rel)
		if err != nil {
			errs = append(errs, err)
			c.Logger.Error("upload failed", "file", rel, "err", err)
			return nil
		}
		c.seen[rel] = st
		c.dirty = true
		if st.Size <= c.MaxSize {
			uploaded++
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	} else {
		// Forget files that are gone, so memory follows the directory.
		for rel := range c.seen {
			if !present[rel] {
				delete(c.seen, rel)
				c.dirty = true
			}
		}
	}
	if err := c.save(); err != nil {
		errs = append(errs, fmt.Errorf("state file: %w", err))
	}
	return uploaded, errors.Join(errs...)
}

// upload reads rel and puts it. The stamp comes from the open file, so it
// describes exactly the bytes that were sent.
func (c *Collector) upload(ctx context.Context, root *os.Root, rel string) (stamp, error) {
	f, err := root.Open(rel)
	if err != nil {
		return stamp{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return stamp{}, err
	}
	st := stamp{Size: info.Size(), MTime: info.ModTime().UnixNano()}
	if !info.Mode().IsRegular() {
		return stamp{}, fmt.Errorf("not a regular file")
	}
	if info.Size() > c.MaxSize {
		c.Logger.Warn("file too large, not uploading", "file", rel, "size", info.Size(), "max", c.MaxSize)
		return st, nil
	}
	body, err := io.ReadAll(io.LimitReader(f, c.MaxSize+1))
	if err != nil {
		return stamp{}, err
	}
	if int64(len(body)) > c.MaxSize {
		return stamp{}, fmt.Errorf("grew past %d bytes while reading", c.MaxSize)
	}
	key := filepath.ToSlash(rel)
	if p := strings.Trim(c.Prefix, "/"); p != "" {
		key = p + "/" + key
	}
	ctype := mime.TypeByExtension(filepath.Ext(rel))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	put, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.Store.Put(put, key, ctype, body); err != nil {
		return stamp{}, err
	}
	c.Logger.Info("uploaded", "file", rel, "key", key, "size", len(body))
	return st, nil
}

// save writes the state file atomically when something changed.
func (c *Collector) save() error {
	if c.StateFile == "" || !c.dirty {
		return nil
	}
	b, err := json.Marshal(c.seen)
	if err != nil {
		return err
	}
	tmp := c.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.StateFile); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// Run sweeps every interval until ctx ends, then sweeps once more, ignoring
// Settle and retrying failures, until drain runs out.
func (c *Collector) Run(ctx context.Context, interval, drain time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.Sweep(ctx, false)
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), drain)
			defer cancel()
			for attempt := 0; ; attempt++ {
				n, err := c.Sweep(final, true)
				if err == nil {
					c.Logger.Info("drained", "uploaded", n)
					return
				}
				select {
				case <-final.Done():
					c.Logger.Error("drain incomplete", "err", err)
					return
				case <-time.After(time.Duration(attempt+1) * time.Second):
				}
			}
		}
	}
}
