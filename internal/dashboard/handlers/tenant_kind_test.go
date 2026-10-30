package handlers

// RP82-7 (Stage 1a Prompt 53, Part 0d): the admin's totals and lists count
// customers by default — a platform's child and a sandbox are tenants rows
// too, and were counted as accounts. ?kind=live|sandbox|child|all.

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAdminTenants_KindFilter(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	mark := fmt.Sprintf("kindf%d", time.Now().UnixNano())
	live, child, sandbox := mark+"-live", mark+"-child", mark+"-sandbox"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tenants (id, name, email, platform) VALUES ($1, $1, $2, TRUE)`, []any{live, live + "@test.local"}},
		{`INSERT INTO tenants (id, name, email, parent_tenant_id) VALUES ($1, $1, $2, $3)`, []any{child, child + "@platform.invalid", live}},
		{`INSERT INTO tenants (id, name, email, sandbox_of) VALUES ($1, $1, $2, $3)`, []any{sandbox, sandbox + "@sandbox.invalid", live}},
	} {
		_, err := db.Exec(q.sql, q.args...)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenants WHERE id IN ($1, $2)`, child, sandbox)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, live)
	})
	list := func(query string) string {
		w := httptest.NewRecorder()
		HandleTenantList(testAdminTenantsTemplate(t), db, zap.NewNop()).ServeHTTP(w,
			httptest.NewRequest("GET", "/admin/tenants?q="+mark+query, nil).WithContext(adminCtx(t)))
		require.Equal(t, 200, w.Code)
		return w.Body.String()
	}
	shown := func(body string) (out []string) {
		for _, id := range []string{live, child, sandbox} {
			if strings.Contains(body, id) {
				out = append(out, id)
			}
		}
		return out
	}
	assert.Equal(t, []string{live}, shown(list("")), "the default is the customers")
	assert.Equal(t, []string{child}, shown(list("&kind=child")))
	assert.Equal(t, []string{sandbox}, shown(list("&kind=sandbox")))
	assert.Equal(t, []string{live, child, sandbox}, shown(list("&kind=all")))
	assert.Equal(t, []string{live}, shown(list("&kind=nonsense")), "an unknown kind is the default")

	for kind, want := range map[string]string{"": "t.sandbox_of IS NULL AND t.parent_tenant_id IS NULL",
		"child": "t.parent_tenant_id IS NOT NULL", "sandbox": "t.sandbox_of IS NOT NULL", "all": "TRUE"} {
		assert.Equal(t, want, tenantKindSQL(parseTenantKind(kind), "t"), kind)
	}
}
