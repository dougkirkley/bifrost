package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/relay"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// RelaysHandler serves the Relays management API, the usage preview and
// the Relay Logs API. Every write goes through the config store first, so the vault and
// encryption hooks see the credential, then the runtime manager is updated.
type RelaysHandler struct {
	store      configstore.ConfigStore
	manager    *relay.Manager
	config     *lib.Config
	vkReloader VirtualKeyReloader
	// writeMu serializes endpoint writes, so the live endpoint, the stored row and a rollback after a
	// failed save always come from the same request.
	writeMu sync.Mutex
}

// NewRelaysHandler builds the handler. store may be nil when the config store is off,
// in which case writes are refused.
func NewRelaysHandler(store configstore.ConfigStore, manager *relay.Manager, config *lib.Config, vkReloader VirtualKeyReloader) *RelaysHandler {
	return &RelaysHandler{store: store, manager: manager, config: config, vkReloader: vkReloader}
}

// RegisterRoutes mounts the management and logs routes behind the dashboard middleware.
func (h *RelaysHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/relays", lib.ChainMiddlewares(h.list, middlewares...))
	r.POST("/api/relays", lib.ChainMiddlewares(h.create, middlewares...))
	r.POST("/api/relays/usage-preview", lib.ChainMiddlewares(h.preview, middlewares...))
	r.GET("/api/relays/{name}", lib.ChainMiddlewares(h.get, middlewares...))
	r.PUT("/api/relays/{name}", lib.ChainMiddlewares(h.update, middlewares...))
	r.DELETE("/api/relays/{name}", lib.ChainMiddlewares(h.delete, middlewares...))

	r.GET("/api/relay-logs", lib.ChainMiddlewares(h.searchLogs, middlewares...))
	r.DELETE("/api/relay-logs", lib.ChainMiddlewares(h.deleteLogs, middlewares...))
	r.GET("/api/relay-logs/stats", lib.ChainMiddlewares(h.logStats, middlewares...))
	r.GET("/api/relay-logs/filterdata", lib.ChainMiddlewares(h.logFilterData, middlewares...))
	r.GET("/api/relay-logs/histogram", lib.ChainMiddlewares(h.logHistogram, middlewares...))
	r.GET("/api/relay-logs/top-endpoints", lib.ChainMiddlewares(h.topEndpoints, middlewares...))
	r.GET("/api/relay-logs/{id}", lib.ChainMiddlewares(h.getLog, middlewares...))
}

// relayView is what the API returns for an endpoint: the config with every secret
// redacted.
type relayView struct {
	*schemas.RelayConfig
	URL string `json:"url"` // the path callers use, /relay/{name}
}

func endpointView(cfg *schemas.RelayConfig) relayView {
	return relayView{RelayConfig: cfg.Redacted(), URL: "/relay/" + cfg.Name}
}

func (h *RelaysHandler) requireStore(ctx *fasthttp.RequestCtx) bool {
	if h.store == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "config store is disabled: relays cannot be managed")
		return false
	}
	return true
}

func (h *RelaysHandler) list(ctx *fasthttp.RequestCtx) {
	workCtx, cancel := RequestWorkContext(ctx, RequestWorkTimeout)
	defer cancel()
	if !h.requireStore(ctx) {
		return
	}
	// search matches the name or description case-insensitively; limit and offset page the matches,
	// so a picker never fetches the whole list to filter it. Without limit every match is returned.
	args := ctx.QueryArgs()
	search := strings.ToLower(strings.TrimSpace(string(args.Peek("search"))))
	limit, offset := 0, 0
	if raw := string(args.Peek("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > relaysMaxPageSize {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid limit: must be between 1 and %d", relaysMaxPageSize))
			return
		}
		limit = n
	}
	if raw := string(args.Peek("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "invalid offset: must be zero or more")
			return
		}
		offset = n
	}
	cfgs, err := h.store.ListRelays(workCtx)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to list relays")
		return
	}
	matches := make([]relayView, 0, len(cfgs))
	for _, cfg := range cfgs {
		if search != "" && !strings.Contains(strings.ToLower(cfg.Name), search) && !strings.Contains(strings.ToLower(cfg.Description), search) {
			continue
		}
		matches = append(matches, endpointView(cfg))
	}
	page := matches[min(offset, len(matches)):]
	if limit > 0 && len(page) > limit {
		page = page[:limit]
	}
	SendJSON(ctx, map[string]any{"relays": page, "count": len(page), "total_count": len(matches)})
}

// relaysMaxPageSize bounds one page of the relay list.
const relaysMaxPageSize = 1000

func (h *RelaysHandler) get(ctx *fasthttp.RequestCtx) {
	workCtx, cancel := RequestWorkContext(ctx, RequestWorkTimeout)
	defer cancel()
	if !h.requireStore(ctx) {
		return
	}
	cfg, err := h.store.GetRelay(workCtx, pathName(ctx))
	if err != nil {
		h.storeError(ctx, err)
		return
	}
	SendJSON(ctx, endpointView(cfg))
}

// ReloadFromStore recompiles every endpoint from the store, under the same lock as endpoint writes so
// a rebuild never installs a snapshot older than a save that ran alongside it. It runs after the
// global proxy changes, since an endpoint without its own proxy resolved the inherited one when built.
func (h *RelaysHandler) ReloadFromStore(ctx context.Context) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.store == nil {
		return nil
	}
	cfgs, err := h.store.ListRelays(ctx)
	if err != nil {
		return err
	}
	return h.manager.Replace(cfgs)
}

// rollbackCreate removes a just-created row that could not go live. It runs under its own bounded
// context, since the request's may be what timed out, and logs a failure rather than hide it: the
// row would then stay stored but unserved until it is deleted.
func (h *RelaysHandler) rollbackCreate(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestWorkTimeout)
	defer cancel()
	if err := h.store.DeleteRelay(ctx, name); err != nil {
		logger.Error("relay %s: create failed and removing the stored row also failed: %v", name, err)
	}
}

func (h *RelaysHandler) create(ctx *fasthttp.RequestCtx) {
	workCtx, cancel := RequestWorkContext(ctx, RequestWorkTimeout)
	defer cancel()
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if !h.requireStore(ctx) {
		return
	}
	var cfg schemas.RelayConfig
	if err := json.Unmarshal(ctx.PostBody(), &cfg); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid relay: "+err.Error())
		return
	}
	cfg.Name = strings.TrimSpace(cfg.Name)
	if err := cfg.ValidateEndpoint(); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.CreateRelay(workCtx, &cfg, ""); err != nil {
		h.storeError(ctx, err)
		return
	}
	stored, err := h.store.GetRelay(workCtx, cfg.Name)
	if err != nil {
		// A row that cannot be read back is not served, so it is not kept either: a retry would
		// otherwise meet a conflict for an endpoint that never went live.
		h.rollbackCreate(cfg.Name)
		h.storeError(ctx, err)
		return
	}
	if err := h.manager.Upsert(stored); err != nil {
		// The row is valid by construction; a compile failure here (a bad CA, a proxy that does not
		// parse) is reported, and the stored row is rolled back so the two never disagree.
		h.rollbackCreate(cfg.Name)
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	lib.RecordAuditChange(ctx, "relay.created", "relay", cfg.Name, cfg.Name, nil, endpointView(stored))
	SendJSONWithStatus(ctx, endpointView(stored), fasthttp.StatusCreated)
}

func (h *RelaysHandler) update(ctx *fasthttp.RequestCtx) {
	workCtx, cancel := RequestWorkContext(ctx, RequestWorkTimeout)
	defer cancel()
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if !h.requireStore(ctx) {
		return
	}
	name := pathName(ctx)
	previous, err := h.store.GetRelay(workCtx, name)
	if err != nil {
		h.storeError(ctx, err)
		return
	}
	var cfg schemas.RelayConfig
	if err := json.Unmarshal(ctx.PostBody(), &cfg); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid relay: "+err.Error())
		return
	}
	if cfg.Name != "" && cfg.Name != name {
		SendError(ctx, fasthttp.StatusBadRequest, "a relay's name cannot be changed")
		return
	}
	cfg.Name = name
	// A secret sent back in its redacted form keeps the stored value, so the UI can save an edit
	// without ever holding the credential.
	cfg.MergeRedacted(previous)
	if err := cfg.ValidateEndpoint(); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	if err := h.manager.Upsert(&cfg); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.UpdateRelay(workCtx, &cfg, ""); err != nil {
		if rbErr := h.manager.Upsert(previous); rbErr != nil {
			logger.Error("relay %s: save failed and restoring the previous live config also failed: %v", name, rbErr)
		}
		h.storeError(ctx, err)
		return
	}
	lib.RecordAuditChange(ctx, "relay.updated", "relay", name, name, endpointView(previous), endpointView(&cfg))
	SendJSON(ctx, endpointView(&cfg))
}

func (h *RelaysHandler) delete(ctx *fasthttp.RequestCtx) {
	workCtx, cancel := RequestWorkContext(ctx, RequestWorkTimeout)
	defer cancel()
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if !h.requireStore(ctx) {
		return
	}
	name := pathName(ctx)
	previous, err := h.store.GetRelay(workCtx, name)
	if err != nil {
		h.storeError(ctx, err)
		return
	}
	// Read the grants before deleting, so the keys whose cascade-deleted grants just disappeared
	// stop carrying them in memory.
	granted, err := h.store.GetRelayGrantedVirtualKeyIDs(workCtx, name)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to read relay grants")
		return
	}
	if err := h.store.DeleteRelay(workCtx, name); err != nil {
		h.storeError(ctx, err)
		return
	}
	h.manager.Remove(name)
	h.reloadVirtualKeys(workCtx, granted)
	lib.RecordAuditChange(ctx, "relay.deleted", "relay", name, name, endpointView(previous), nil)
	ctx.SetStatusCode(fasthttp.StatusNoContent)
}

func (h *RelaysHandler) reloadVirtualKeys(ctx context.Context, ids []string) {
	if h.vkReloader == nil {
		return
	}
	for _, id := range ids {
		if _, err := h.vkReloader.ReloadVirtualKey(ctx, id); err != nil {
			logger.Error("failed to reload virtual key %s after relay delete: %v", id, err)
		}
	}
}

func (h *RelaysHandler) storeError(ctx *fasthttp.RequestCtx, err error) {
	switch {
	case errors.Is(err, configstore.ErrNotFound):
		SendError(ctx, fasthttp.StatusNotFound, "relay not found")
	case errors.Is(err, configstore.ErrRelayExists):
		SendError(ctx, fasthttp.StatusConflict, err.Error())
	default:
		logger.Error("relay store error: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to save relay")
	}
}

// usagePreviewRequest checks a mapping against a sample call. Config is an unsaved endpoint; Name
// previews a saved one instead.
type usagePreviewRequest struct {
	Name   string                  `json:"name,omitempty"`
	Config *schemas.RelayConfig    `json:"config,omitempty"`
	Sample relay.UsagePreviewInput `json:"sample"`
}

func (h *RelaysHandler) preview(ctx *fasthttp.RequestCtx) {
	workCtx, cancel := RequestWorkContext(ctx, RequestWorkTimeout)
	defer cancel()
	var req usagePreviewRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid preview request: "+err.Error())
		return
	}
	cfg := req.Config
	if cfg == nil {
		if req.Name == "" || h.store == nil {
			SendError(ctx, fasthttp.StatusBadRequest, "config or the name of a saved endpoint is required")
			return
		}
		stored, err := h.store.GetRelay(workCtx, req.Name)
		if err != nil {
			h.storeError(ctx, err)
			return
		}
		cfg = stored
	}
	out, err := relay.PreviewUsage(cfg, req.Sample)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSON(ctx, out)
}

// --- Relay Logs ---

func (h *RelaysHandler) logsStore(ctx *fasthttp.RequestCtx) logstore.LogStore {
	if h.config == nil || h.config.LogsStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "logs store is not available")
		return nil
	}
	return h.config.LogsStore
}

const relayLogsMaxLimit = 1000

func parseRelayLogFilters(ctx *fasthttp.RequestCtx) (logstore.RelayLogFilters, error) {
	args := ctx.QueryArgs()
	value := func(name string) string { return strings.TrimSpace(string(args.Peek(name))) }
	values := func(name string) []string { return parseAgentHistoryMultiValue(args, name) }
	f := logstore.RelayLogFilters{
		EndpointNames: values("relay_names"), Methods: values("methods"), Statuses: values("statuses"), Kinds: values("kinds"),
		VirtualKeyIDs: values("virtual_key_ids"), UserIDs: values("user_ids"), TeamIDs: values("team_ids"),
		CustomerIDs: values("customer_ids"), BusinessUnitIDs: values("business_unit_ids"), ProjectIDs: values("project_ids"),
		RequestID: value("request_id"), JobID: value("job_id"), Search: value("search"),
	}
	if raw := value("billable"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return f, fmt.Errorf("invalid billable: must be true or false")
		}
		f.Billable = &b
	}
	for name, target := range map[string]**time.Time{"start_time": &f.StartTime, "end_time": &f.EndTime} {
		if raw := value(name); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return f, fmt.Errorf("invalid %s: must be RFC3339", name)
			}
			*target = &parsed
		}
	}
	for name, target := range map[string]**float64{"min_cost": &f.MinCost, "max_cost": &f.MaxCost} {
		if raw := value(name); raw != "" {
			parsed, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return f, fmt.Errorf("invalid %s", name)
			}
			*target = &parsed
		}
	}
	return f, nil
}

func (h *RelaysHandler) searchLogs(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	filters, err := parseRelayLogFilters(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	pagination := logstore.PaginationOptions{Limit: 50, SortBy: string(ctx.QueryArgs().Peek("sort_by")), Order: string(ctx.QueryArgs().Peek("order"))}
	if raw := string(ctx.QueryArgs().Peek("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > relayLogsMaxLimit {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid limit: must be between 1 and %d", relayLogsMaxLimit))
			return
		}
		pagination.Limit = limit
	}
	if raw := string(ctx.QueryArgs().Peek("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "invalid offset: must be a non-negative number")
			return
		}
		pagination.Offset = offset
	}
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	result, err := store.SearchRelayLogs(logCtx, filters, pagination)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to search relay logs")
		return
	}
	SendJSON(ctx, result)
}

func (h *RelaysHandler) getLog(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	id, _ := ctx.UserValue("id").(string)
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	entry, err := store.FindRelayLog(logCtx, id)
	if err != nil {
		if errors.Is(err, logstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, "relay log not found")
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to read relay log")
		return
	}
	SendJSON(ctx, entry)
}

func (h *RelaysHandler) deleteLogs(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil || len(req.IDs) == 0 {
		SendError(ctx, fasthttp.StatusBadRequest, "ids are required")
		return
	}
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	if err := store.DeleteRelayLogs(logCtx, req.IDs); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to delete relay logs")
		return
	}
	SendJSON(ctx, map[string]any{"deleted": len(req.IDs)})
}

func (h *RelaysHandler) logStats(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	filters, err := parseRelayLogFilters(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	stats, err := store.GetRelayLogStats(logCtx, filters)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to get relay log stats")
		return
	}
	SendJSON(ctx, stats)
}

func (h *RelaysHandler) logHistogram(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	filters, err := parseRelayLogFilters(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	bucket := int64(3600)
	if raw := string(ctx.QueryArgs().Peek("bucket_size_seconds")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "invalid bucket_size_seconds")
			return
		}
		bucket = parsed
	}
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	result, err := store.GetRelayHistogram(logCtx, filters, bucket)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to get relay log histogram")
		return
	}
	SendJSON(ctx, result)
}

func (h *RelaysHandler) topEndpoints(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	filters, err := parseRelayLogFilters(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	limit := 10
	if raw := string(ctx.QueryArgs().Peek("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > relayLogsMaxLimit {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid limit: must be between 1 and %d", relayLogsMaxLimit))
			return
		}
		limit = parsed
	}
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	result, err := store.GetRelayTopEndpoints(logCtx, filters, limit)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to rank relays")
		return
	}
	SendJSON(ctx, result)
}

func (h *RelaysHandler) logFilterData(ctx *fasthttp.RequestCtx) {
	store := h.logsStore(ctx)
	if store == nil {
		return
	}
	logCtx := agentGatewayLogContext(ctx)
	defer logCtx.Cancel()
	result, err := store.GetRelayFilterData(logCtx, 100)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to load relay log filters")
		return
	}
	SendJSON(ctx, result)
}
