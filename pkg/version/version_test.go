package version

import "testing"

func TestDirtySuffix(t *testing.T) {
	old := Commit
	defer func() { Commit = old }()
	Commit = "8ac478a2107763df27edb2206eb9c6bf8d1d0132-dirty"
	i := Get()
	if i.Commit != "8ac478a2107763df27edb2206eb9c6bf8d1d0132" || !i.Dirty {
		t.Fatalf("got %+v", i)
	}
	if got := i.Short(); got != "dev (8ac478a21077-dirty, built "+i.Date+")" {
		t.Fatalf("short: %s", got)
	}
}
