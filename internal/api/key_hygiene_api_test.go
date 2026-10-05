package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R5-12 on the entry points: an expired key answers its own code —
// ExpiredToken on S3 (with the key's expiry in the message, not the
// presigned URL's wording), a typed 401 on the JSON API that takes a key.

func TestS3Auth_ExpiredKeyAnswersExpiredToken(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	k, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "short-lived", &auth.KeyCreateOptions{ExpiresAt: &future})
	require.NoError(t, err)
	require.Equal(t, "", f.put(k.Key, k.Secret, "before-expiry.txt"))

	// Expire it (SetAPIKeyExpiration persists; the S3 path reads the row).
	require.NoError(t, f.svc.SetAPIKeyExpiration(ctx, f.user.ID, k.ID, time.Now().Add(-time.Minute)))

	_, err = f.client(k.Key, k.Secret).PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(f.bucket), Key: aws.String("after-expiry.txt"), Body: strings.NewReader("late")})
	require.Error(t, err)
	assert.Equal(t, ErrExpiredPresignedRequest, s3ErrorCode(err), "ExpiredToken, not AccessDenied")
	assert.Contains(t, err.Error(), "access key expired at", "the message names the key's expiry")
}

func TestSTSRoute_ExpiredParentIsATyped401(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	parent, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "parent", &auth.KeyCreateOptions{ExpiresAt: &future})
	require.NoError(t, err)
	require.NoError(t, f.svc.SetAPIKeyExpiration(ctx, f.user.ID, parent.ID, time.Now().Add(-time.Minute)))

	req := httptest.NewRequest("POST", "/api/v1/sts/token", strings.NewReader(`{"permissions":["PutObject"],"ttl":600,"parent_key_id":"`+parent.ID+`"}`))
	c := context.WithValue(req.Context(), userIDKey, f.user.ID)
	c = context.WithValue(c, tenantIDKey, f.tenant.ID)
	rr := httptest.NewRecorder()
	f.srv.handleSTSCreateToken(rr, req.WithContext(c))

	require.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
	var resp struct {
		Error struct {
			Type, Code, Param string
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, ErrTypeAuthentication, resp.Error.Type)
	assert.Equal(t, "parent_key_expired", resp.Error.Code)
	assert.Equal(t, "parent_key_id", resp.Error.Param)
}
