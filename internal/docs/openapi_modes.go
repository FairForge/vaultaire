package docs

import (
	"strings"

	"github.com/FairForge/vaultaire/internal/modes"
)

// The OpenAPI text of a bucket's mode and of the S3 encryption headers is
// GENERATED from modes.json (keystone K3, contract line 3): nothing here
// types a mode's name or what it lets us read. A sentence that is true only
// with the master key loaded (the Standard customer sentence) is rendered from
// modes.MasterKeyLoaded() — the spec is built once at route setup, after
// the key store started.

// modeEnumDescription is ManagedBucket.mode's description: every mode's id,
// name and customer sentence.
func modeEnumDescription() string {
	var b strings.Builder
	b.WriteString("The bucket's mode (a property of the surface, set by the bucket's settings): ")
	for i, m := range modes.All().Modes {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString("`" + m.ID + "` — " + m.Name + ": " + m.Customer.Render(modes.MasterKeyLoaded()))
	}
	return b.String()
}

// modeIDs are the enum's values, in the table's order.
func modeIDs() []string {
	var ids []string
	for _, m := range modes.All().Modes {
		ids = append(ids, m.ID)
	}
	return ids
}

// createModeIDs are the modes a bucket can be created in through the API
// (modes.DeveloperModes).
func createModeIDs() []string { return modes.DeveloperModes() }

// createModeDescription is ManagedBucketCreateRequest.mode's description:
// each creatable mode's name and how a bucket has it.
func createModeDescription() string {
	var b strings.Builder
	b.WriteString("The bucket's mode, set at creation (omitted = the default, `" + modes.All().Surfaces["developer"].Default + "`): ")
	for i, id := range modes.DeveloperModes() {
		m, _ := modes.Get(id)
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString("`" + m.ID + "` — " + m.Name + ": " + m.Customer.Render(modes.MasterKeyLoaded()))
	}
	return b.String()
}

// headerParam is an optional request header.
func headerParam(name, desc string) Parameter {
	return Parameter{Name: name, In: "header", Description: desc, Schema: &Schema{Type: "string"}}
}

// encryptionHeaderParams are PutObject's server-side-encryption headers,
// described from the Standard and Private rows of modes.json.
func encryptionHeaderParams() []Parameter {
	std, _ := modes.Get(modes.Standard)
	priv, _ := modes.Get(modes.Private)
	sse := "`AES256` asks for server-side encryption under a key we hold (" + std.Name + "). "
	if modes.MasterKeyLoaded() {
		sse += std.Customer.Render(true)
	} else {
		sse += "This deployment has no master key loaded, so such a write is refused (503) and nothing is stored. " +
			std.Name + " today: " + std.Customer.Render(false)
	}
	ssec := "SSE-C (" + priv.Name + "): " + priv.Customer.Render(modes.MasterKeyLoaded()) + " "
	return []Parameter{
		headerParam("x-amz-server-side-encryption", sse),
		headerParam("x-amz-server-side-encryption-customer-algorithm", ssec+"Value `AES256`; with the key and its MD5 below on every request."),
		headerParam("x-amz-server-side-encryption-customer-key", ssec+"The base64 AES-256 key; never stored."),
		headerParam("x-amz-server-side-encryption-customer-key-MD5", ssec+"The base64 MD5 of the key."),
	}
}
