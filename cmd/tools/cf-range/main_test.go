package main

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func fakeLookup(m map[string][]string) func(string) ([]net.IP, error) {
	return func(h string) ([]net.IP, error) {
		v, ok := m[h]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []net.IP
		for _, s := range v {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func TestCFRange_RefusesAProxiedControlPlane(t *testing.T) {
	lookup := fakeLookup(map[string][]string{
		"stored.ge":    {"104.21.5.9", "2606:4700:3030::6815:509"}, // Cloudflare's
		"s3.stored.ge": {"203.0.113.10"},                           // DNS-only: the origin
		"mixed.test":   {"203.0.113.11", "172.67.1.1"},
	})
	var out bytes.Buffer
	assert.Equal(t, 1, run([]string{"https://stored.ge"}, lookup, &out), out.String())
	assert.Contains(t, out.String(), "104.21.5.9")
	out.Reset()
	assert.Equal(t, 0, run([]string{"https://s3.stored.ge/"}, lookup, &out), out.String())
	assert.Equal(t, 0, run([]string{"s3.stored.ge"}, lookup, &out), "a bare host")
	assert.Equal(t, 1, run([]string{"mixed.test"}, lookup, &out), "one Cloudflare address is enough")
	assert.Equal(t, 0, run([]string{"http://127.0.0.1:8000"}, lookup, &out), "an IP is checked as itself")
	assert.Equal(t, 1, run([]string{"https://104.16.0.1"}, lookup, &out))
	assert.Equal(t, 2, run([]string{"https://nowhere.test"}, lookup, &out), "unresolvable")
	assert.Equal(t, 2, run(nil, lookup, &out), "usage")
}
