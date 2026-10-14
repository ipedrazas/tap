package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Minted by tap's pkg/egress.Mint for the same claims and key.
const tapToken = "eyJhIjoiZWNoby1hZ2VudCIsInMiOiJjb2xsZWN0b3I6c2Vzc2lvbnMiLCJlIjoxNzkxMDAwMDAwLCJuIjoiMDAxMTIyMzNhYWJiY2NkZCJ9.mGJXJH6YjzvPxj_OrGiW4YziEA4z0l-cl4WuTs8Z4uE"

func TestEgressTokenMatchesTap(t *testing.T) {
	tok, err := mintEgressToken([]byte("0123456789abcdef"), egressClaims{Agent: "echo-agent", Scope: "collector:sessions", Expires: 1791000000, Nonce: "00112233aabbccdd"})
	if err != nil {
		t.Fatal(err)
	}
	if tok != tapToken {
		t.Errorf("token\n got %s\nwant %s", tok, tapToken)
	}
}

func TestEgressProxyURL(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := egressProxy("egress:3128", "echo-agent", "collector:sessions", keyFile)
	a, err := proxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := proxy(nil)
	pass, _ := a.User.Password()
	if a.Scheme != "http" || a.Host != "egress:3128" || a.User.Username() != "echo-agent" || strings.Count(pass, ".") != 1 {
		t.Fatalf("proxy URL %s", a.Redacted())
	}
	if a.String() == b.String() {
		t.Error("two connections got the same credential")
	}
}
