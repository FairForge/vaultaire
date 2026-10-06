package drivers

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// StoreID (engine.StoreIdentifier, WP-R7-5): the store each driver writes
// into, as one comparable string. The boot check reports two registered
// names that resolve to the same store — the invariant every stale-copy
// delete relies on (WP-R13-2). Two iDrive drivers on one bucket name are two
// stores when their endpoints differ (the fixed bucket exists per region).

func (d *IDriveDriver) StoreID() string { return "s3://" + hostOf(d.endpoint) + "/" + d.bucket }

func (d *LyveDriver) StoreID() string {
	return fmt.Sprintf("s3://s3.%s.global.lyve.seagate.com/%s", d.region, d.getBucket())
}

func (d *GeyserDriver) StoreID() string { return "s3://" + hostOf(d.endpoint) + "/" + d.bucket }

func (d *R2Driver) StoreID() string { return "s3://" + hostOf(d.endpoint) + "/" + d.bucket }

func (d *S3CompatDriver) StoreID() string {
	return "s3://us.s3compat.cloud:8000/" + d.bucket + "/" + d.prefix
}

// S3Driver is container-keyed (one bucket per container): the store is the
// endpoint.
func (d *S3Driver) StoreID() string { return "s3://" + hostOf(d.endpoint) }

func (d *QuotalessDriver) StoreID() string { return "s3://" + hostOf(d.endpoint) + "/" + d.rootPath }

func (d *LocalDriver) StoreID() string {
	abs, err := filepath.Abs(d.basePath)
	if err != nil {
		abs = d.basePath
	}
	return "dir:" + abs
}

// A WebDAV backend: the server and the root folder objects live under.
func (d *WebDAVDriver) StoreID() string { return "dav:" + d.origin + d.escapedPath(nil, true) }

// The fleet: the sorted set of its account ids.
func (d *OneDriveDriver) StoreID() string {
	ids := make([]string, 0, len(d.tenants))
	for _, t := range d.tenants {
		ids = append(ids, t.tenantID)
	}
	sort.Strings(ids)
	return "graph:" + strings.Join(ids, ",")
}

func hostOf(endpoint string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	return strings.TrimSuffix(s, "/")
}
