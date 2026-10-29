package api

// WP-TEAM-1 (Stage 1a Prompt 52, PR B): invitations. A link lives 7 days,
// works once, and only its token's hash is stored; the owner is permanent.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// captureSender keeps the e-mails the server sends.
type captureSender struct{ to, html []string }

func (c *captureSender) Send(_ context.Context, to, _, html, _ string) error {
	c.to, c.html = append(c.to, to), append(c.html, html)
	return nil
}

// tokenIn is the invitation token of a sent e-mail's link.
func tokenIn(t *testing.T, html string) string {
	t.Helper()
	i := strings.Index(html, "/invitations/")
	require.GreaterOrEqual(t, i, 0, "the e-mail carries the link")
	rest := html[i+len("/invitations/"):]
	end := strings.IndexAny(rest, "\"<\n ")
	require.Greater(t, end, 0)
	return rest[:end]
}

// invitee is a user (with no tenant of its own) at address, and a JWT for it.
func (f *platformFixture) invitee(address string) (*auth.User, string) {
	f.t.Helper()
	u := &auth.User{ID: uuid.NewString(), Email: address}
	_, err := f.db.Exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u.ID, u.Email)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, u.ID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1 OR performed_by = $1`, u.ID)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
	})
	jwt, err := f.s.auth.GenerateJWTForTenant(u, "", "")
	require.NoError(f.t, err)
	return u, jwt
}

func TestMembers_InvitationExpiresIn7Days(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	mail := &captureSender{}
	f.s.emailSender, f.s.baseURL = mail, "https://stored.example"
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID) })
	address := "invitee-" + uuid.NewString()[:8] + "@stored.ge"
	invitee, inviteeJWT := f.invitee(address)

	// Invite: one e-mail with the link; the row holds the token's hash only,
	// and expires 7 days after it was made.
	code, out := f.call(http.MethodPost, "/api/v1/manage/members", f.jwt, map[string]any{"email": address, "role": "viewer"})
	require.Equal(t, http.StatusCreated, code, out)
	require.Len(t, mail.to, 1)
	assert.Equal(t, address, mail.to[0])
	token := tokenIn(t, mail.html[0])
	var hash string
	var ttl time.Duration
	var seconds float64
	require.NoError(t, f.db.QueryRow(`SELECT token_hash, EXTRACT(EPOCH FROM expires_at - created_at) FROM tenant_invitations WHERE tenant_id = $1`,
		f.user.TenantID).Scan(&hash, &seconds))
	ttl = time.Duration(seconds) * time.Second
	assert.Equal(t, 7*24*time.Hour, ttl)
	assert.Equal(t, auth.HashInvitationToken(token), hash, "the hash is stored")
	assert.NotContains(t, hash, token, "never the token")

	// Day 8: 410 invitation_expired, and no member was made.
	_, err := f.db.Exec(`UPDATE tenant_invitations SET created_at = NOW() - INTERVAL '8 days', expires_at = NOW() - INTERVAL '1 day' WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	code, out = f.call(http.MethodPost, "/api/v1/manage/invitations/"+token+"/accept", inviteeJWT, nil)
	assert.Equal(t, http.StatusGone, code, out)
	assert.Equal(t, "invitation_expired", errCode(out))
	_, err = auth.MemberRole(context.Background(), f.db, f.user.TenantID, invitee.ID)
	require.ErrorIs(t, err, auth.ErrNotMember)

	// Within the 7 days it is accepted once; a second use is 410 invitation_used.
	_, err = f.db.Exec(`UPDATE tenant_invitations SET created_at = NOW(), expires_at = NOW() + INTERVAL '7 days' WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	code, out = f.call(http.MethodPost, "/api/v1/manage/invitations/"+token+"/accept", inviteeJWT, nil)
	require.Equal(t, http.StatusOK, code, out)
	role, err := auth.MemberRole(context.Background(), f.db, f.user.TenantID, invitee.ID)
	require.NoError(t, err)
	assert.Equal(t, auth.RoleViewer, role)
	code, out = f.call(http.MethodPost, "/api/v1/manage/invitations/"+token+"/accept", inviteeJWT, nil)
	assert.Equal(t, http.StatusGone, code, out)
	assert.Equal(t, "invitation_used", errCode(out))
}

// The invitation names its address: another signed-in user cannot spend it.
func TestMembers_InvitationIsTheAddressees(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	mail := &captureSender{}
	f.s.emailSender = mail
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID) })
	address := "addressee-" + uuid.NewString()[:8] + "@stored.ge"
	code, out := f.call(http.MethodPost, "/api/v1/manage/members", f.jwt, map[string]any{"email": address, "role": "developer"})
	require.Equal(t, http.StatusCreated, code, out)
	_, otherJWT := f.invitee("someone-else-" + uuid.NewString()[:8] + "@stored.ge")
	code, out = f.call(http.MethodPost, "/api/v1/manage/invitations/"+tokenIn(t, mail.html[0])+"/accept", otherJWT, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "invitation_other_address", errCode(out))
}

// The owner row is never removed or demoted; an admin manages the others;
// a developer manages no one.
func TestMembers_TheOwnerIsPermanent(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.s.emailSender = &captureSender{}
	_, admin := f.addMember(auth.RoleAdmin)
	viewer, _ := f.addMember(auth.RoleViewer)
	_, developer := f.addMember(auth.RoleDeveloper)

	code, out := f.call(http.MethodDelete, "/api/v1/manage/members/"+f.user.ID, admin, nil)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "owner_is_permanent", errCode(out))
	code, out = f.call(http.MethodPatch, "/api/v1/manage/members/"+f.user.ID, admin, map[string]any{"role": "viewer"})
	assert.Equal(t, http.StatusConflict, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/manage/members", admin, map[string]any{"email": "x@stored.ge", "role": "owner"})
	assert.Equal(t, http.StatusBadRequest, code, "no invitation makes an owner: %v", out)

	code, out = f.call(http.MethodPatch, "/api/v1/manage/members/"+viewer.ID, developer, map[string]any{"role": "admin"})
	assert.Equal(t, http.StatusForbidden, code, out)
	code, out = f.call(http.MethodPatch, "/api/v1/manage/members/"+viewer.ID, admin, map[string]any{"role": "developer"})
	assert.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+viewer.ID, admin, nil)
	assert.Equal(t, http.StatusOK, code, out)
	_, err := auth.MemberRole(context.Background(), f.db, f.user.TenantID, viewer.ID)
	assert.ErrorIs(t, err, auth.ErrNotMember, "removed: the membership, never the user or a tenant")
	var users int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, viewer.ID).Scan(&users))
	assert.Equal(t, 1, users)
}
