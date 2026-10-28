package dataplane

import (
	"context"
	"fmt"
)

// The kinds of data-plane host (data_plane_hosts.kind, migration 112).
const (
	KindEdge = "edge" // the Cloudflare Worker on s3.stored.cloud
	KindNode = "node" // this binary with VAULTAIRE_ROLE=node
	KindSLC  = "slc"  // the origin's own data path (host=slc)
)

// HostSLC is the registry's name of the origin itself.
const HostSLC = "slc"

// HostInfo is the data-plane host a control-plane call came from: its
// registry name and kind, and — for a tenant-scoped (BYO) host — the one
// tenant it may admit, whose reports are never trusted for integrity or
// metering (RP68-3; the 2026-10-26 COGS rule).
type HostInfo struct {
	Name     string
	Kind     string
	TenantID string
}

// TenantScoped says whether the host admits one tenant only.
func (h HostInfo) TenantScoped() bool { return h.TenantID != "" }

type hostKey struct{}

// WithHost puts the calling host on ctx (the control plane's edge-call
// gate, or the origin's in-process control plane).
func WithHost(ctx context.Context, h HostInfo) context.Context {
	return context.WithValue(ctx, hostKey{}, h)
}

// HostFrom is the calling host on ctx, and whether one was put there.
func HostFrom(ctx context.Context) (HostInfo, bool) {
	h, ok := ctx.Value(hostKey{}).(HostInfo)
	return h, ok
}

// StatusError is a control-plane call that answered another status than
// 200: the call's own refusal (its JSON error), never an S3 decision.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("control plane answered %d: %s", e.Status, e.Msg)
}
