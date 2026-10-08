package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/mcp"
	"github.com/ipedrazas/tap/pkg/spec"
)

// cmdMCPSnapshot pins each MCP server's allowlisted tool schemas into
// mcp/<server>.tools.json. With --check it only reports drift (exit 1).
func cmdMCPSnapshot(args []string) error {
	fs := flag.NewFlagSet("mcp snapshot", flag.ContinueOnError)
	check := fs.Bool("check", false, "compare the live schemas with the pinned ones instead of writing")
	envFile := fs.String("env-file", "", "KEY=value secrets for MCP auth (default .env.<agent>)")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, err := spec.Load(dir)
	if err != nil {
		return err
	}
	if len(b.Agent.MCP) == 0 {
		fmt.Println("no mcp servers declared")
		return nil
	}
	if *envFile == "" {
		*envFile = ".env." + b.Agent.Metadata.Name
	}
	secrets, err := readEnv(*envFile)
	if err != nil {
		return err
	}
	drifted := false
	for _, s := range b.Agent.MCP {
		header := http.Header{}
		if s.Auth.Type != "none" {
			v, ok := secrets[s.Auth.Secret]
			if !ok {
				return fmt.Errorf("%s: %s is not set in %s", s.Name, s.Auth.Secret, *envFile)
			}
			if s.Auth.Type == "bearer" {
				header.Set("Authorization", "Bearer "+v)
			} else {
				header.Set(s.Auth.Header, v)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		c := &mcp.Client{URL: s.URL, Header: header, HTTP: &http.Client{Timeout: 60 * time.Second}}
		live, err := c.ListTools(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		var allow []string
		for _, t := range s.Tools {
			allow = append(allow, t.Name)
		}
		fmt.Printf("%s: server offers %d tools, %d allowlisted\n", s.Name, len(live), len(allow))
		for _, t := range live {
			if !slices.Contains(allow, t.Name) {
				fmt.Printf("  not allowlisted: %s\n", t.Name)
			}
		}
		warnEffects(s, live)
		path := filepath.Join(dir, spec.MCPSnapshotPath(s.Name))
		if *check {
			old, ok := b.MCPSnapshots[s.Name]
			if !ok {
				fmt.Printf("  %s missing\n", spec.MCPSnapshotPath(s.Name))
				drifted = true
				continue
			}
			pinned := mcp.Snapshot(old)
			for _, name := range mcp.Drift(pinned, live) {
				fmt.Printf("  drift: %s\n", name)
				drifted = true
			}
			continue
		}
		snap, err := mcp.Pin(live, allow)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(snap); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote %s\n", path)
	}
	if drifted {
		return &exitError{code: 1, msg: "pinned MCP schemas are stale; review the change, re-run `tapctl mcp snapshot` and bump metadata.version"}
	}
	return nil
}

// warnEffects flags allowlisted tools whose server annotations contradict
// the declared effects level. Annotations are untrusted hints, so this warns.
func warnEffects(s spec.MCPServer, live []mcp.Tool) {
	byName := map[string]mcp.Tool{}
	for _, t := range live {
		byName[t.Name] = t
	}
	for _, t := range s.Tools {
		a := byName[t.Name].Annotations
		if a == nil || t.Effects != spec.EffectRead {
			continue
		}
		if (a.ReadOnlyHint != nil && !*a.ReadOnlyHint) || (a.DestructiveHint != nil && *a.DestructiveHint) {
			fmt.Printf("  warning: %s is declared effects: read but the server says it is not read-only\n", t.Name)
		}
	}
}

func readEnv(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out, sc.Err()
}

// cmdMCPCall makes one live tools/call and prints the raw CallToolResult, so
// fixtures can record real mcp_response bodies. It refuses tools outside the
// allowlist.
func cmdMCPCall(args []string) error {
	fs := flag.NewFlagSet("mcp call", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "KEY=value secrets for MCP auth (default .env.<agent>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 3 {
		return fmt.Errorf("usage: tapctl mcp call [--env-file f] <agent-dir> <server>__<tool> '<json args>'")
	}
	dir, full, rawArgs := rest[0], rest[1], rest[2]
	b, err := spec.Load(dir)
	if err != nil {
		return err
	}
	server, tool, _ := strings.Cut(full, "__")
	var srv *spec.MCPServer
	for i, s := range b.Agent.MCP {
		if s.Name == server {
			for _, t := range s.Tools {
				if t.Name == tool {
					srv = &b.Agent.MCP[i]
				}
			}
		}
	}
	if srv == nil {
		return fmt.Errorf("%s is not an allowlisted MCP tool of %s", full, b.Agent.Metadata.Name)
	}
	var callArgs map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &callArgs); err != nil {
		return fmt.Errorf("args: %w", err)
	}
	if *envFile == "" {
		*envFile = ".env." + b.Agent.Metadata.Name
	}
	secrets, err := readEnv(*envFile)
	if err != nil {
		return err
	}
	header := http.Header{}
	switch srv.Auth.Type {
	case "bearer":
		header.Set("Authorization", "Bearer "+secrets[srv.Auth.Secret])
	case "header":
		header.Set(srv.Auth.Header, secrets[srv.Auth.Secret])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c := &mcp.Client{URL: srv.URL, Header: header, HTTP: &http.Client{}}
	raw, err := c.CallTool(ctx, tool, callArgs)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return err
	}
	fmt.Println(pretty.String())
	return nil
}

// cmdMCPList prints every tool a server offers (not just the allowlist), with
// the server's own hints, to help choose what to allowlist.
func cmdMCPList(args []string) error {
	fs := flag.NewFlagSet("mcp list", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "KEY=value secrets for MCP auth (default .env.<agent>)")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, err := spec.Load(dir)
	if err != nil {
		return err
	}
	if *envFile == "" {
		*envFile = ".env." + b.Agent.Metadata.Name
	}
	secrets, err := readEnv(*envFile)
	if err != nil {
		return err
	}
	for _, s := range b.Agent.MCP {
		header := http.Header{}
		switch s.Auth.Type {
		case "bearer":
			header.Set("Authorization", "Bearer "+secrets[s.Auth.Secret])
		case "header":
			header.Set(s.Auth.Header, secrets[s.Auth.Secret])
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		live, err := (&mcp.Client{URL: s.URL, Header: header, HTTP: &http.Client{}}).ListTools(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		allowed := map[string]bool{}
		for _, t := range s.Tools {
			allowed[t.Name] = true
		}
		fmt.Printf("%s (%s): %d tools\n", s.Name, s.URL, len(live))
		for _, t := range live {
			mark := " "
			if allowed[t.Name] {
				mark = "*"
			}
			hint := ""
			if a := t.Annotations; a != nil {
				if a.ReadOnlyHint != nil {
					hint += fmt.Sprintf(" readOnly=%v", *a.ReadOnlyHint)
				}
				if a.DestructiveHint != nil {
					hint += fmt.Sprintf(" destructive=%v", *a.DestructiveHint)
				}
			}
			desc, _, _ := strings.Cut(t.Description, "\n")
			if len(desc) > 100 {
				desc = desc[:100] + "…"
			}
			fmt.Printf(" %s %s%s\n     %s\n", mark, t.Name, hint, desc)
		}
		fmt.Println("   (* = allowlisted)")
	}
	return nil
}
