package logging

import (
	"errors"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
)

var _ schemas.RelayPlugin = (*LoggerPlugin)(nil)

// maxEndpointLogBodyLen bounds each body a Relay Logs row keeps. Endpoint payloads can be
// documents or audio; the row keeps enough to debug a mapping, not the payload itself.
const maxEndpointLogBodyLen = 64 * 1024

// PreRelayHook records the request side of a relay call. The row is
// completed and written by the post-hook, which runs once the reply (or the stream) has ended.
func (p *LoggerPlugin) PreRelayHook(ctx *schemas.BifrostContext, req *schemas.BifrostRelayRequest) (*schemas.BifrostRelayRequest, *schemas.RelayPluginShortCircuit, error) {
	if ctx == nil || req == nil {
		return req, nil, nil
	}
	requestID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyRequestID)
	if requestID == "" {
		return req, nil, nil
	}
	// The row id is generated, as for Agent logs: the request id can come from the caller's
	// x-request-id, so two calls may share it. It is kept for correlation only.
	entry := &logstore.RelayLog{
		ID:        uuid.New().String(),
		RequestID: clampString(requestID, 255),
		Timestamp: time.Now().UTC(),
		RelayName: clampString(req.RelayName, 64),
		Kind:      logstore.RelayLogKindCall,
		Method:    clampString(req.Method, 10),
		Path:      req.Path,
		RoutePath: req.RoutePath,
		Billable:  req.Billable,
		Streamed:  req.Streams,
		Model:     clampString(req.Model, 255),
		Status:    "processing",
	}
	if req.RawQuery != "" {
		entry.Path += "?" + redactQuery(req.RawQuery)
	}
	if len(req.Headers) > 0 {
		entry.RequestHeadersParsed = schemas.RedactSensitiveHeaders(maps.Clone(req.Headers))
	}
	if len(req.Body) > 0 {
		entry.RequestBody = boundedBody(req.Body)
	}
	p.pendingEndpointLogs.Store(requestID, entry)
	return req, nil, nil
}

// PostRelayHook completes and writes the Relay Logs row, with the governance
// attribution and the cost breakdown, and records the job row a job route started so the sweeper
// settles it against the same budgets.
func (p *LoggerPlugin) PostRelayHook(ctx *schemas.BifrostContext, resp *schemas.BifrostRelayResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostRelayResponse, *schemas.BifrostError, error) {
	if ctx == nil {
		return resp, bifrostErr, nil
	}
	requestID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyRequestID)
	pending, _ := p.pendingEndpointLogs.LoadAndDelete(requestID)
	entry, _ := pending.(*logstore.RelayLog)
	if entry == nil {
		entry = &logstore.RelayLog{ID: uuid.New().String(), RequestID: clampString(requestID, 255), Timestamp: time.Now().UTC(), Kind: logstore.RelayLogKindCall}
	}
	entry.ApplyGovernanceContext(ctx)
	if resp != nil {
		if entry.RelayName == "" {
			entry.RelayName = clampString(resp.RelayName, 64)
		}
		entry.StatusCode = resp.StatusCode
		entry.Streamed = resp.Streamed
		entry.BytesOut = resp.BytesOut
		latency := float64(resp.Latency)
		entry.Latency = &latency
		if len(resp.Headers) > 0 {
			entry.ResponseHeadersParsed = schemas.RedactSensitiveHeaders(maps.Clone(resp.Headers))
		}
		if len(resp.Body) > 0 {
			entry.ResponseBody = boundedBody(resp.Body)
		}
		applyEndpointUsage(entry, resp.Usage)
	}
	policy := p.resolveContentPolicy(ctx)
	switch {
	case bifrostErr != nil && bifrostErr.Error != nil && bifrostErr.Error.Type != nil && *bifrostErr.Error.Type == schemas.RequestCancelled:
		entry.Status = logstore.RelayLogStatusCancelled
	case bifrostErr != nil || (resp != nil && resp.StatusCode >= 400):
		entry.Status = logstore.RelayLogStatusError
	default:
		entry.Status = logstore.RelayLogStatusSuccess
	}
	if bifrostErr != nil {
		shouldStoreRaw, _ := ctx.Value(schemas.BifrostContextKeyShouldStoreRawInLogs).(bool)
		entry.ErrorDetailsParsed = sanitizeErrorForLogging(bifrostErr, policy.visible(), shouldStoreRaw)
		if entry.StatusCode == 0 && bifrostErr.StatusCode != nil {
			entry.StatusCode = *bifrostErr.StatusCode
		}
	}
	entry.ContentHidden = policy.hidden
	// Relay rows have no object-storage offload yet, so hidden content is dropped rather than
	// kept in columns the logs API would return. Query values are caller content too.
	if !policy.storeContent || policy.hidden {
		entry.RequestBody, entry.ResponseBody = nil, nil
		entry.RequestHeadersParsed, entry.ResponseHeadersParsed = nil, nil
		if i := strings.IndexByte(entry.Path, '?'); i >= 0 {
			entry.Path = entry.Path[:i]
		}
	}
	entry.PluginLogs = serializePluginLogsBounded(ctx.GetPluginLogs(), maxA2APluginLogsLen)
	if resp != nil && resp.Usage != nil && resp.Usage.Job != nil {
		p.recordEndpointJob(entry, resp.Usage.Job)
	}
	p.enqueueEndpointLogEntry(entry)
	return resp, bifrostErr, nil
}

// applyEndpointUsage copies a counted call's cost breakdown onto its row.
func applyEndpointUsage(entry *logstore.RelayLog, usage *schemas.RelayUsage) {
	if usage == nil {
		return
	}
	entry.Cost = schemas.Ptr(usage.Cost)
	entry.CostPerRequest = schemas.Ptr(usage.CostPerRequest)
	entry.CostPerUnit = schemas.Ptr(usage.CostPerUnit)
	entry.ExtractedCost = usage.ExtractedCost
	entry.Units = usage.Units
	entry.BilledUnits = usage.BilledUnits
	entry.InputTokens, entry.OutputTokens, entry.TotalTokens = usage.InputTokens, usage.OutputTokens, usage.TotalTokens
	if usage.Job != nil {
		entry.JobID = schemas.Ptr(usage.Job.ID)
		entry.JobStatus = schemas.Ptr("started")
	}
}

// maxEndpointJobGenerations bounds the dedupe lookup for one upstream job id reused after settlement;
// past it each start gets a row keyed by its own request, which is settled but no longer deduped.
const maxEndpointJobGenerations = 64

// recordEndpointJob writes the job row the sweeper settles. The row is keyed by endpoint and upstream
// job id, so a caller retrying an idempotent start (the upstream answers with the same id while the
// job runs) records nothing new and is billed once. An id the upstream reuses after the first job was
// settled is a new job: it takes the next generation slot, so retries of that job dedupe against the
// same slot instead of each start opening a row of its own.
func (p *LoggerPlugin) recordEndpointJob(entry *logstore.RelayLog, job *schemas.RelayJobHandle) {
	if p.batchStore == nil || job == nil || job.ID == "" {
		return
	}
	kind := tables.ProviderJobKindRelay
	var scoped, id string
	for gen := 1; ; gen++ {
		if gen > maxEndpointJobGenerations {
			scoped = schemas.RelayScopedJobID(entry.RequestID, job.ID)
			id = tables.ProviderJobID(kind, entry.RelayName, scoped)
			break
		}
		prefix := ""
		if gen > 1 {
			prefix = "g" + strconv.Itoa(gen)
		}
		scoped = schemas.RelayScopedJobID(prefix, job.ID)
		id = tables.ProviderJobID(kind, entry.RelayName, scoped)
		existing, err := p.batchStore.GetProviderJob(p.ctx, id)
		if errors.Is(err, configstore.ErrNotFound) || (err == nil && existing == nil) {
			break // a free slot
		}
		if err != nil {
			// Recording on a lookup error could open a second row for an idempotent retry.
			p.logger.Warn("failed to look up relay job endpoint=%s job_id=%s: %v", entry.RelayName, job.ID, err)
			return
		}
		if existing.AccountingStatus != tables.ProviderJobAccountingStatusAccounted && existing.AccountingStatus != tables.ProviderJobAccountingStatusUnpriceable {
			return // an idempotent retry of a start whose job is still open
		}
	}
	next := time.Now().UTC().Add(time.Duration(max(job.IntervalSeconds, 1)) * time.Second)
	params := job.Params
	row := &tables.TableProviderJob{
		ID:             id,
		Kind:           kind,
		Provider:       entry.RelayName,
		JobID:          scoped,
		Endpoint:       entry.RoutePath,
		Params:         &params,
		ProviderStatus: "started",
		NextCheckAt:    &next,
		VirtualKeyID:   entry.VirtualKeyID,
		UserID:         entry.UserID,
		TeamID:         entry.TeamID,
		CustomerID:     entry.CustomerID,
		SourceLogID:    schemas.Ptr(entry.ID),
	}
	if len(entry.BudgetIDsParsed) > 0 {
		if data, err := sonic.MarshalString(entry.BudgetIDsParsed); err == nil {
			row.BudgetIDs = &data
		}
	}
	if len(entry.RateLimitIDsParsed) > 0 {
		if data, err := sonic.MarshalString(entry.RateLimitIDsParsed); err == nil {
			row.RateLimitIDs = &data
		}
	}
	if err := p.batchStore.UpsertProviderJob(p.ctx, row); err != nil {
		p.logger.Warn("failed to record relay job endpoint=%s job_id=%s: %v", entry.RelayName, job.ID, err)
	}
}

// sensitiveQueryKeys are query parameter names that carry credentials but no header pattern names.
var sensitiveQueryKeys = map[string]struct{}{"key": {}, "token": {}, "apikey": {}, "password": {}, "pwd": {}, "sig": {}, "signature": {}}

// redactQuery masks the values of credential-bearing query parameters (api_key, access_token, ...)
// before a query is stored with the path, keeping the parameter order and names. A key is
// sensitive when the header rules name it (with "_" read as "-") or it is a well-known credential
// parameter.
func redactQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		name, _, hasValue := strings.Cut(part, "=")
		if !hasValue {
			continue
		}
		key, err := url.QueryUnescape(name)
		if err != nil {
			key = name
		}
		normalized := strings.ToLower(strings.ReplaceAll(key, "_", "-"))
		if _, ok := sensitiveQueryKeys[strings.ReplaceAll(normalized, "-", "")]; ok || schemas.IsSensitiveHeader(normalized) {
			parts[i] = name + "=" + url.QueryEscape(schemas.RedactedAttrValue)
		}
	}
	return strings.Join(parts, "&")
}

// boundedBody keeps up to maxEndpointLogBodyLen bytes of a body as text; a binary body is not kept.
func boundedBody(body []byte) *string {
	if len(body) > maxEndpointLogBodyLen {
		body = body[:maxEndpointLogBodyLen]
	}
	if !utf8.Valid(body) {
		// A cut can split a rune at the end; anything else invalid is a binary payload.
		trimmed := body
		for i := 0; i < utf8.UTFMax && len(trimmed) > 0 && !utf8.Valid(trimmed); i++ {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if !utf8.Valid(trimmed) {
			return nil
		}
		body = trimmed
	}
	s := string(body)
	return &s
}
