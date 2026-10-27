package configstore

import (
	"context"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func testRelay(name string) *schemas.RelayConfig {
	return &schemas.RelayConfig{
		Name:         name,
		Description:  "search API",
		BaseURL:      "https://search.example.com",
		Auth:         schemas.RelayAuthConfig{Type: schemas.RelayAuthBearer},
		Credential:   &schemas.RelayCredential{Value: schemas.NewSecretVar("tvly-secret-value")},
		ExtraHeaders: map[string]schemas.SecretVar{"X-Tenant": *schemas.NewSecretVar("acme")},
		Usage:        &schemas.RelayUsageConfig{CostPerRequest: schemas.Ptr(0.001)},
		Routes:       []schemas.RelayRouteConfig{{Method: "POST", Path: "/search", Usage: &schemas.RelayUsageConfig{CostPerUnit: schemas.Ptr(0.008), UnitsPath: "usage.credits"}}},
	}
}

func TestRelayPersistence(t *testing.T) {
	ctx := context.Background()
	store := setupRDBTestStore(t)

	require.NoError(t, store.CreateRelay(ctx, testRelay("tavily"), "hash-1"))
	err := store.CreateRelay(ctx, testRelay("tavily"), "")
	require.ErrorIs(t, err, ErrRelayExists)

	got, err := store.GetRelay(ctx, "tavily")
	require.NoError(t, err)
	assert.Equal(t, "tvly-secret-value", got.Credential.Value.GetValue(), "the credential round-trips")
	tenant := got.ExtraHeaders["X-Tenant"]
	assert.Equal(t, "acme", tenant.GetValue())
	require.Len(t, got.Routes, 1)
	assert.InDelta(t, 0.008, *got.Routes[0].Usage.CostPerUnit, 1e-12, "per-route overrides round-trip")

	var raw tables.TableRelay
	require.NoError(t, store.DB().Session(&gorm.Session{SkipHooks: true}).First(&raw, "name = ?", "tavily").Error)
	assert.Equal(t, tables.EncryptionStatusEncrypted, raw.EncryptionStatus)
	require.NotNil(t, raw.CredentialValue)
	assert.NotEqual(t, "tvly-secret-value", raw.CredentialValue.Val, "the credential is encrypted at rest")
	assert.False(t, strings.Contains(raw.ExtraHeadersJSON, "acme"), "extra header values are encrypted at rest")
	assert.False(t, strings.Contains(raw.ConfigJSON, "tvly-secret-value"), "and never land in the plain config column")

	// An API update keeps the stored config hash; a file sync replaces it.
	got.Description = "edited"
	require.NoError(t, store.UpdateRelay(ctx, got, ""))
	hashes, err := store.GetRelayConfigHashes(ctx)
	require.NoError(t, err)
	assert.Equal(t, "hash-1", hashes["tavily"])
	require.NoError(t, store.UpdateRelay(ctx, got, "hash-2"))
	hashes, err = store.GetRelayConfigHashes(ctx)
	require.NoError(t, err)
	assert.Equal(t, "hash-2", hashes["tavily"])

	reloaded, err := store.GetRelay(ctx, "tavily")
	require.NoError(t, err)
	assert.Equal(t, "edited", reloaded.Description)
	assert.Equal(t, "tvly-secret-value", reloaded.Credential.Value.GetValue(), "an update re-encrypts and still decrypts")

	missing := testRelay("missing")
	require.ErrorIs(t, store.UpdateRelay(ctx, missing, ""), ErrNotFound)

	list, err := store.ListRelays(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
}
