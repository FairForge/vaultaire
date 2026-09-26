package testutil

import "testing"

func TestParseURL(t *testing.T) {
	c, err := parseURL("postgres://viera:pw@dbhost:5433/vaultaire?sslmode=require")
	if err != nil || c.Host != "dbhost" || c.Port != 5433 || c.User != "viera" || c.Password != "pw" || c.Database != "vaultaire" || c.SSLMode != "require" {
		t.Fatalf("%+v %v", c, err)
	}
	c, _ = parseURL("postgres://viera@localhost:5432/vaultaire?sslmode=disable")
	if c.Password != "" || c.Database != "vaultaire" || c.SSLMode != "disable" {
		t.Fatalf("%+v", c)
	}
	if _, err := parseURL("mysql://x/y"); err == nil {
		t.Fatal("expected scheme error")
	}
}
