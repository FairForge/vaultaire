package translog

import (
	"context"
	"fmt"
)

// FoundBuild is the log entry that lists a build's sha256 (FindBuild).
type FoundBuild struct {
	Index int64
	Entry Entry // ServerBuild or ClientBuild
}

// String is one line for a verifier's output.
func (f FoundBuild) String() string {
	switch e := f.Entry.(type) {
	case ClientBuild:
		return fmt.Sprintf("client %s %s listed at entry %d — inclusion OK; source commit %s, toolchain %s, recipe %s",
			e.Client, e.Version, f.Index, e.SourceCommit, e.Toolchain, e.Recipe)
	case ServerBuild:
		return fmt.Sprintf("server build %s listed at entry %d — inclusion OK; built %s with %s",
			e.GitSHA, f.Index, e.BuildTime, e.GoVersion)
	}
	return fmt.Sprintf("%s listed at entry %d — inclusion OK", f.Entry.Kind(), f.Index)
}

// FindBuild proves that an artefact with this sha256 (hex) is a build the
// log lists under the verified checkpoint cp — the server (a server_build
// entry's binary_sha256; client == "") or the named client (a client_build
// entry's sha256 with that client name) — with the inclusion proof of the
// entry checked. It walks the entry bundles the checkpoint commits to; an
// artefact no entry lists is an error: that is the refusal (K4c-3b, plan
// 43.6 "clients refuse an unlisted build"; WP-SUPPLY-1 release-sign verify
// -log). translog-verify -client and release-sign share it: one reader.
func FindBuild(ctx context.Context, c *Client, cp Checkpoint, sha256Hex, client string) (FoundBuild, error) {
	for n := int64(0); n*TileWidth < cp.Size; n++ {
		_, bodies, err := c.EntryBundle(ctx, cp, n)
		if err != nil {
			return FoundBuild{}, fmt.Errorf("entry bundle %d: %w", n, err)
		}
		for i, b := range bodies {
			d, err := Decode(b)
			if err != nil {
				continue // an entry kind this build does not know
			}
			var match bool
			switch e := d.Entry.(type) {
			case ClientBuild:
				match = client != "" && e.Client == client && e.SHA256 == sha256Hex
			case ServerBuild:
				match = client == "" && e.BinarySHA256 == sha256Hex
			}
			if !match {
				continue
			}
			idx := n*TileWidth + int64(i)
			if _, err := c.VerifyInclusion(ctx, cp, idx); err != nil {
				return FoundBuild{}, fmt.Errorf("inclusion of the %s entry %d: %w", d.Entry.Kind(), idx, err)
			}
			return FoundBuild{Index: idx, Entry: d.Entry}, nil
		}
	}
	if client != "" {
		return FoundBuild{}, fmt.Errorf("build not in the log: no client_build entry for %s lists sha256 %s in the %d entries the checkpoint commits to — refuse this build", client, sha256Hex, cp.Size)
	}
	return FoundBuild{}, fmt.Errorf("build not in the log: no server_build entry lists sha256 %s in the %d entries the checkpoint commits to — refuse this build", sha256Hex, cp.Size)
}
