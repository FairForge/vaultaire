package drivers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The routing-truth boot check compares stores: two names on one WebDAV
// folder are one store; another root or another server is another.
func TestWebDAVDriver_StoreID(t *testing.T) {
	mk := func(url, root string) string {
		d, err := NewWebDAVDriver("sync", url, "u", "p", root, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d.StoreID()
	}
	assert.Equal(t, "dav:http://127.0.0.1:4918/vaultaire/", mk("http://127.0.0.1:4918", "vaultaire"))
	assert.Equal(t, mk("http://127.0.0.1:4918/", "/vaultaire/"), mk("http://127.0.0.1:4918", "vaultaire"))
	assert.NotEqual(t, mk("http://127.0.0.1:4918", "a"), mk("http://127.0.0.1:4918", "b"))
	assert.NotEqual(t, mk("http://127.0.0.1:4918", "a"), mk("http://127.0.0.1:4919", "a"))
}

func TestSyncWebDAVConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	_, ok := SyncWebDAVConfigFromEnv(env(nil))
	assert.False(t, ok, "no password = no sync driver")

	c, ok := SyncWebDAVConfigFromEnv(env(map[string]string{"SYNC_WEBDAV_PASSWORD": "pw"}))
	assert.True(t, ok)
	assert.Equal(t, WebDAVConfig{URL: "http://127.0.0.1:4918", User: "sync", Password: "pw", Root: "vaultaire"}, c)

	c, ok = SyncWebDAVConfigFromEnv(env(map[string]string{
		"SYNC_WEBDAV_PASSWORD": "pw", "SYNC_WEBDAV_URL": "http://127.0.0.1:5000/",
		"SYNC_WEBDAV_USER": "other", "SYNC_WEBDAV_ROOT": "stored/prod",
	}))
	assert.True(t, ok)
	assert.Equal(t, WebDAVConfig{URL: "http://127.0.0.1:5000/", User: "other", Password: "pw", Root: "stored/prod"}, c)
}
