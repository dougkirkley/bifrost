package governance

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/jobaccounting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pteTestVKValue = "sk-bf-pte"

// newPluginForEndpoints builds a governance plugin holding one virtual key with a budget and a rate
// limit, granted the named endpoints (or every endpoint with allowAll).
func newPluginForEndpoints(t *testing.T, allowAll bool, endpoints ...string) (*GovernancePlugin, *LocalGovernanceStore) {
	t.Helper()
	logger := NewMockLogger()
	budget := buildBudgetWithUsage("pte-budget", 100.0, 0.0, "1d")
	rl := buildRateLimit("pte-rl", 1_000_000, 1_000)
	vk := buildVirtualKeyWithBudget("vk-pte", pteTestVKValue, "pte-vk", budget)
	vk.RateLimit = rl
	vk.RateLimitID = &rl.ID
	vk.AllowAllRelays = allowAll
	for _, name := range endpoints {
		vk.RelayGrants = append(vk.RelayGrants, configstoreTables.TableVirtualKeyRelayGrant{VirtualKeyID: vk.ID, RelayName: name})
	}
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		Budgets:     []configstoreTables.TableBudget{*budget},
		RateLimits:  []configstoreTables.TableRateLimit{*rl},
	}, nil, nil)
	require.NoError(t, err)
	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
	return plugin, store
}

func endpointReq(name string) *schemas.BifrostRelayRequest {
	return &schemas.BifrostRelayRequest{RelayName: name, Method: "POST", Path: "/search", Billable: true, RouteIndex: -1}
}

func TestPreRelayHookGrants(t *testing.T) {
	p, _ := newPluginForEndpoints(t, false, "tavily")

	_, short, err := p.PreRelayHook(a2aVKCtx(pteTestVKValue), endpointReq("tavily"))
	require.NoError(t, err)
	assert.Nil(t, short, "a granted endpoint is allowed")

	ctx := a2aVKCtx(pteTestVKValue)
	_, short, err = p.PreRelayHook(ctx, endpointReq("textract"))
	require.NoError(t, err)
	require.NotNil(t, short)
	require.NotNil(t, short.Error.StatusCode)
	assert.Equal(t, 403, *short.Error.StatusCode, "an ungranted endpoint is refused")
	assert.Equal(t, string(DecisionRelayBlocked), *short.Error.Type)
	assert.Equal(t, true, ctx.Value(governanceRejectedContextKey))

	all, _ := newPluginForEndpoints(t, true)
	_, short, err = all.PreRelayHook(a2aVKCtx(pteTestVKValue), endpointReq("textract"))
	require.NoError(t, err)
	assert.Nil(t, short, "allow_all_relays grants every endpoint")

	none, _ := newPluginForEndpoints(t, false)
	_, short, err = none.PreRelayHook(a2aVKCtx(pteTestVKValue), endpointReq("tavily"))
	require.NoError(t, err)
	require.NotNil(t, short, "a key with no endpoint grant is denied by default")

	_, short, err = none.PreRelayHook(anonymousA2ACtx(), endpointReq("tavily"))
	require.NoError(t, err)
	assert.Nil(t, short, "a request with no credential and no mandatory key is unrestricted, as for MCP tools")
}

func TestPostRelayHookChargesOverallLimits(t *testing.T) {
	p, store := newPluginForEndpoints(t, false, "tavily")
	ctx := a2aVKCtx(pteTestVKValue)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-pte-1")
	_, short, err := p.PreRelayHook(ctx, endpointReq("tavily"))
	require.NoError(t, err)
	require.Nil(t, short)

	resp := &schemas.BifrostRelayResponse{RelayName: "tavily", StatusCode: 200, Usage: &schemas.RelayUsage{Cost: 0.033}}
	_, _, err = p.PostRelayHook(ctx, resp, nil)
	require.NoError(t, err)

	// An uncounted call (no usage) charges nothing.
	ctx2 := a2aVKCtx(pteTestVKValue)
	ctx2.SetValue(schemas.BifrostContextKeyRequestID, "req-pte-2")
	_, _, err = p.PreRelayHook(ctx2, endpointReq("tavily"))
	require.NoError(t, err)
	_, _, err = p.PostRelayHook(ctx2, &schemas.BifrostRelayResponse{RelayName: "tavily", StatusCode: 200}, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		data := store.GetGovernanceData(context.Background())
		return data.Budgets["pte-budget"].CurrentUsage > 0
	}, 2*time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	data := store.GetGovernanceData(context.Background())
	assert.InDelta(t, 0.033, data.Budgets["pte-budget"].CurrentUsage, 1e-9, "the formula cost lands on the key's budget")
	assert.EqualValues(t, 1, data.RateLimits["pte-rl"].RequestCurrentUsage, "one counted call, the uncounted one is not")
	assert.Equal(t, []string{"pte-budget"}, ctx.Value(schemas.BifrostContextKeyGovernanceBudgetIDs), "the charged budgets are recorded for the log and job rows")
}

func TestReportEndpointUsageIsIdempotent(t *testing.T) {
	p, store := newPluginForEndpoints(t, false, "textract")
	report := jobaccounting.RelayUsageReport{RequestID: "settle-1", RelayName: "textract", Cost: 0.15, BudgetIDs: []string{"pte-budget"}, RateLimitIDs: []string{"pte-rl"}}
	require.NoError(t, p.ReportRelayUsage(context.Background(), report))
	require.NoError(t, p.ReportRelayUsage(context.Background(), report))
	data := store.GetGovernanceData(context.Background())
	assert.InDelta(t, 0.15, data.Budgets["pte-budget"].CurrentUsage, 1e-9, "a retried settlement charges once")
	assert.EqualValues(t, 0, data.RateLimits["pte-rl"].RequestCurrentUsage, "a settlement counts no request; the start call did")
}
