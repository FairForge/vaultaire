package main

import (
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	const tn = "tenant-14a623b16b3f7012"
	cases := []struct {
		name      string
		tenant    string
		container string
		artifact  string
		want      item
	}{
		{"addressed object", tn, tn + "_sync-lab", "wl/k@gzkz279a70av24rcprv7vamsfx",
			item{Kind: kindObject, Bucket: "sync-lab", Key: "wl/k"}},
		{"object at its key", tn, tn + "_sync-lab", "wl-040959/race/k",
			item{Kind: kindObject, Bucket: "sync-lab", Key: "wl-040959/race/k"}},
		{"an @ that is no address", tn, tn + "_b", "mail@example.com",
			item{Kind: kindObject, Bucket: "b", Key: "mail@example.com"}},
		{"id outside the alphabet", tn, tn + "_b", "k@gzkz279a70av24rcprv7vamsfu",
			item{Kind: kindObject, Bucket: "b", Key: "k@gzkz279a70av24rcprv7vamsfu"}},
		{"system bucket", tn, tn + "__exports", "e/1.json",
			item{Kind: kindObject, Bucket: "_exports", Key: "e/1.json"}},
		{"parity shard", tn, tn + "__parity", "e12b45c64ca7d4145a9c6cc4/a83485b8-4/p2",
			item{Kind: kindParity, ShardPrefix: "e12b45c64ca7d4145a9c6cc4/a83485b8-4"}},
		{"parity, not a shard", tn, tn + "__parity", "e12b45c64ca7d4145a9c6cc4/x",
			item{Kind: kindOther}},
		{"stripe piece", tn, tn + "_big%p", "ab/abcd-g20261022-x/p00001",
			item{Kind: kindStripe}},
		{"blomp segment", tn, "_segments", "gen/0001",
			item{Kind: kindSegment}},
		{"pack", "_global", "_packs", "ab/abcdef.pack",
			item{Kind: kindPack, Key: "ab/abcdef.pack"}},
		{"canary", "_probe", "_probe", "slc-vaultaire-01",
			item{Kind: kindCanary}},
		{"another tenant's container", tn, "tenant-other_b", "k",
			item{Kind: kindOther}},
		{"no container", tn, "", "k",
			item{Kind: kindOther}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.tenant, engine.TenantObject{Container: c.container, Artifact: c.artifact})
			c.want.Tenant, c.want.Container, c.want.Artifact = c.tenant, c.container, c.artifact
			assert.Equal(t, c.want, got)
		})
	}
}

func TestTenantFolders(t *testing.T) {
	body := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
<D:response><D:href>/objects/</D:href></D:response>
<D:response><D:href>/objects/t-tenant-14a623b16b3f7012/</D:href></D:response>
<D:response><D:href>/objects/t-_global</D:href></D:response>
<D:response><D:href>/objects/_ops/</D:href></D:response>
<D:response><D:href>/objects/t-tenant-357e3a63382e091f/</D:href></D:response>
<D:response><D:href>/elsewhere/t-nope/</D:href></D:response>
</D:multistatus>`
	got := tenantFolders(body, "objects")
	assert.Equal(t, []string{"tenant-14a623b16b3f7012", "_global", "tenant-357e3a63382e091f"}, got)
}
