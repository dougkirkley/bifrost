package relay

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
)

// PluginPipeline is the narrow view of the core plugin pipeline the relay needs, declared here so
// this package never imports the core bifrost package. It is the Relays analogue of
// agent.PluginPipeline.
type PluginPipeline interface {
	RunRelayPreHooks(ctx *schemas.BifrostContext, req *schemas.BifrostRelayRequest, entered func(int)) (*schemas.BifrostRelayRequest, *schemas.RelayPluginShortCircuit, int)
	RunRelayPostHooks(ctx *schemas.BifrostContext, resp *schemas.BifrostRelayResponse, bifrostErr *schemas.BifrostError, runFrom int) (*schemas.BifrostRelayResponse, *schemas.BifrostError)
}

// Manager holds the configured endpoints and relays calls to them. Reads take one atomic load;
// writes build a new map and swap it in, so a request never sees a half-updated endpoint.
type Manager struct {
	endpoints atomic.Pointer[map[string]*endpoint]
	writeMu   sync.Mutex

	pipelineMu      sync.RWMutex
	acquirePipeline func() PluginPipeline
	releasePipeline func(PluginPipeline)

	// globalProxy returns the proxy an endpoint without its own inherits, nil for none.
	globalProxy func() *schemas.ProxyConfig
	logger      schemas.Logger
}

// NewManager returns an empty manager.
func NewManager(logger schemas.Logger) *Manager {
	m := &Manager{logger: logger}
	empty := map[string]*endpoint{}
	m.endpoints.Store(&empty)
	return m
}

// SetPluginPipeline wires the relay to the core plugin pipeline. With none set, calls are relayed
// without hooks (no governance, no logging).
func (m *Manager) SetPluginPipeline(acquire func() PluginPipeline, release func(PluginPipeline)) {
	m.pipelineMu.Lock()
	defer m.pipelineMu.Unlock()
	m.acquirePipeline, m.releasePipeline = acquire, release
}

// SetGlobalProxy sets the source of the proxy inherited by endpoints with no proxy_config. It takes
// effect on the next Upsert of each endpoint.
func (m *Manager) SetGlobalProxy(fn func() *schemas.ProxyConfig) {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	m.globalProxy = fn
}

// Upsert validates and compiles cfg and makes it live, replacing an endpoint of the same name.
func (m *Manager) Upsert(cfg *schemas.RelayConfig) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if cfg == nil {
		return fmt.Errorf("relay config is required")
	}
	proxy := cfg.ProxyConfig
	if proxy == nil && m.globalProxy != nil {
		proxy = m.globalProxy()
	}
	e, err := newEndpoint(cfg, proxy)
	if err != nil {
		return err
	}
	old := *m.endpoints.Load()
	next := make(map[string]*endpoint, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[cfg.Name] = e
	m.endpoints.Store(&next)
	return nil
}

// Remove takes an endpoint out of service. Calls already in flight finish on the old instance.
func (m *Manager) Remove(name string) {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	old := *m.endpoints.Load()
	if _, ok := old[name]; !ok {
		return
	}
	next := make(map[string]*endpoint, len(old))
	for k, v := range old {
		if k != name {
			next[k] = v
		}
	}
	m.endpoints.Store(&next)
}

// Replace swaps the whole set, for a resync from the config store. Endpoints that fail to compile
// are skipped and reported; the rest go live.
func (m *Manager) Replace(cfgs []*schemas.RelayConfig) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	next := make(map[string]*endpoint, len(cfgs))
	var errs []string
	for _, cfg := range cfgs {
		if cfg == nil {
			continue
		}
		proxy := cfg.ProxyConfig
		if proxy == nil && m.globalProxy != nil {
			proxy = m.globalProxy()
		}
		e, err := newEndpoint(cfg, proxy)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", cfg.Name, err))
			continue
		}
		next[cfg.Name] = e
	}
	m.endpoints.Store(&next)
	if len(errs) > 0 {
		return fmt.Errorf("relays skipped: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Names lists the live endpoints, sorted.
func (m *Manager) Names() []string {
	current := *m.endpoints.Load()
	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) get(name string) *endpoint {
	return (*m.endpoints.Load())[name]
}

// Request is one caller request to /relay/{Endpoint}{Path}. Headers are lower-cased with the
// caller's credentials and gateway headers already stripped.
type Request struct {
	Endpoint string
	Method   string
	Path     string // decoded, rooted
	RawQuery string
	Headers  map[string]string
	Body     []byte
}

// Result is a relayed call. Exactly one of Response (buffered, post-hooks done) and Stream
// (post-hooks run when the stream ends) is set.
type Result struct {
	Response *schemas.BifrostRelayResponse
	Stream   *Stream
}

func statusError(status int, message string) *schemas.BifrostError {
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     schemas.Ptr(status),
		Error:          &schemas.ErrorField{Message: message},
	}
}

// Serve relays one caller request: it matches the route, runs the Relays pre-hooks
// (which may refuse the call), sends the request, prices the reply and runs the post-hooks. A
// refusal before the hooks (unknown endpoint, no route, unmeasurable body) is returned as an error
// with its status and reaches no hook.
func (m *Manager) Serve(ctx *schemas.BifrostContext, in Request) (*Result, *schemas.BifrostError) {
	e := m.get(in.Endpoint)
	if e == nil {
		return nil, statusError(http.StatusNotFound, fmt.Sprintf("relay %q not found", in.Endpoint))
	}
	if name, dup := ambiguousHeader(in.Headers); dup {
		return nil, statusError(http.StatusBadRequest, fmt.Sprintf("header %s is set under two spellings; send it once", name))
	}
	// The model is read under the content type the upstream receives: an operator's Content-Type
	// replaces the caller's on the wire.
	contentType := in.Headers["content-type"]
	if ct, ok := e.operatorHeaders["content-type"]; ok {
		contentType = ct
	}
	p := e.plan(in.Method, in.Path, in.Headers)
	if !p.allowed {
		if p.route != nil {
			return nil, statusError(http.StatusNotFound, fmt.Sprintf("route %s %s on relay %q is disabled", in.Method, in.Path, in.Endpoint))
		}
		return nil, statusError(http.StatusNotFound, fmt.Sprintf("no route for %s %s on relay %q", in.Method, in.Path, in.Endpoint))
	}
	model, err := ReadRequestModel(p.usage.modelSource, in.Body, contentType, in.RawQuery, p.usage.effectiveModelPath())
	if err != nil {
		return nil, statusError(http.StatusBadRequest, err.Error())
	}
	if p.billing {
		if err := p.usage.requestBodyError(in.Body); err != nil {
			return nil, statusError(http.StatusBadRequest, err.Error())
		}
	}
	req := &schemas.BifrostRelayRequest{
		RelayName:  e.cfg.Name,
		Method:     strings.ToUpper(in.Method),
		Path:       in.Path,
		RawQuery:   in.RawQuery,
		Headers:    in.Headers,
		Body:       in.Body,
		Model:      model,
		RouteIndex: p.index,
		Billable:   p.billing,
		Streams:    p.stream == streamOn,
	}
	if p.route != nil {
		req.RoutePath = e.cfg.Routes[p.index].Path
	}
	if requestID, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string); requestID == "" {
		ctx.SetValue(schemas.BifrostContextKeyRequestID, uuid.New().String())
	}
	return m.run(ctx, e, p, req)
}

// run is the hook gate: pre-hooks, the relay, then post-hooks for exactly the plugins whose
// pre-hooks ran. A streamed call keeps the pipeline until the stream ends.
func (m *Manager) run(ctx *schemas.BifrostContext, e *endpoint, p plan, req *schemas.BifrostRelayRequest) (result *Result, finalErr *schemas.BifrostError) {
	m.pipelineMu.RLock()
	acquire, release := m.acquirePipeline, m.releasePipeline
	m.pipelineMu.RUnlock()
	var pipeline PluginPipeline
	if acquire != nil {
		pipeline = acquire()
	}
	released := false
	releaseOnce := func() {
		if !released && pipeline != nil && release != nil {
			released = true
			release(pipeline)
		}
	}

	preCount := 0
	postRun := false
	runPost := func(resp *schemas.BifrostRelayResponse, bErr *schemas.BifrostError) (*schemas.BifrostRelayResponse, *schemas.BifrostError) {
		if postRun || pipeline == nil {
			return resp, bErr
		}
		postRun = true
		return pipeline.RunRelayPostHooks(ctx, resp, bErr, preCount)
	}
	streaming := false
	defer func() {
		if r := recover(); r != nil {
			if m.logger != nil {
				m.logger.Error("recovered from panic in relay %s: %v", req.RelayName, r)
			}
			bErr := statusError(http.StatusInternalServerError, fmt.Sprintf("panic in relay: %v", r))
			_, postErr := runPost(nil, bErr)
			if postErr == nil {
				postErr = bErr
			}
			result, finalErr = nil, postErr
			streaming = false
		}
		if !streaming {
			releaseOnce()
		}
	}()

	if pipeline != nil {
		var shortCircuit *schemas.RelayPluginShortCircuit
		var preReq *schemas.BifrostRelayRequest
		preReq, shortCircuit, preCount = pipeline.RunRelayPreHooks(ctx, req, func(entered int) { preCount = entered })
		if shortCircuit != nil {
			if shortCircuit.Response != nil {
				shortCircuit.Response.RelayName = req.RelayName
			}
			resp, bErr := runPost(shortCircuit.Response, shortCircuit.Error)
			if bErr != nil {
				return nil, bErr
			}
			if resp == nil {
				return nil, statusError(http.StatusInternalServerError, "relay short-circuit carried no response")
			}
			return &Result{Response: resp}, nil
		}
		if preReq != nil {
			req = preReq
		}
	}

	upstream := upstreamRequest{method: req.Method, path: req.Path, rawQuery: req.RawQuery, headers: req.Headers, body: req.Body}
	requestUnits := p.usage.requestUnits(req.Body)

	var reply *bufferedResult
	var bErr *schemas.BifrostError
	if p.stream == streamOff {
		reply, bErr = e.doBuffered(ctx, upstream)
	} else {
		resp, start, openErr := e.openStream(ctx, upstream)
		if openErr != nil {
			return recovered(runPost(nil, openErr))
		}
		if p.stream == streamOn || isEventStream(resp.Header.Get("Content-Type")) {
			streaming = true
			stream := newStream(ctx, e, resp, start, p.usage, p.billing, requestUnits, func(final *schemas.BifrostRelayResponse, bErr *schemas.BifrostError) {
				final.RelayName = req.RelayName
				runPost(final, bErr)
				releaseOnce()
			})
			return &Result{Stream: stream}, nil
		}
		// Not an event stream: read and price it as a buffered reply, whatever the caller asked for.
		reply, bErr = e.readBuffered(resp, start, true)
	}
	if bErr != nil {
		return recovered(runPost(nil, bErr))
	}
	resp := &schemas.BifrostRelayResponse{
		RelayName:  req.RelayName,
		StatusCode: reply.status,
		Headers:    reply.headers,
		Body:       reply.body,
		BytesOut:   int64(len(reply.body)),
		Latency:    reply.latency.Milliseconds(),
	}
	if p.billing && reply.status >= 200 && reply.status < 300 {
		resp.Usage = extractUsage(p.usage, reply.contentType, reply.body, requestUnits)
		// A route that starts a job hands the job id and its settlement snapshot to the logging
		// plugin, which records it for the sweeper. The start call is still counted here.
		resp.Usage.Job = p.route.jobHandle(reply.contentType, reply.body)
	}
	// A post-hook that clears the response without an error is refused like an empty recovery.
	return recovered(runPost(resp, nil))
}

// recovered turns a post-hook outcome into a result: a post-hook may replace an error with a
// response, which is then returned as a buffered reply, and an outcome with neither is refused.
func recovered(resp *schemas.BifrostRelayResponse, bErr *schemas.BifrostError) (*Result, *schemas.BifrostError) {
	if bErr != nil {
		return nil, bErr
	}
	if resp == nil {
		return nil, statusError(http.StatusBadGateway, "relay post-hooks returned neither a response nor an error")
	}
	return &Result{Response: resp}, nil
}

// JobPollResponse is the upstream reply to a job poll.
type JobPollResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// PollJob sends a job poll for the sweeper with the endpoint's credential. It skips the hooks and
// the route check: the poll is the operator's own configured request, not a caller's, and is not
// billed by itself.
func (m *Manager) PollJob(ctx *schemas.BifrostContext, endpointName string, poll JobPollRequest) (*JobPollResponse, *schemas.BifrostError) {
	e := m.get(endpointName)
	if e == nil {
		return nil, statusError(http.StatusNotFound, fmt.Sprintf("relay %q not found", endpointName))
	}
	headers := make(map[string]string, len(poll.Headers))
	for k, v := range poll.Headers {
		headers[k] = v
	}
	reply, bErr := e.doBuffered(ctx, upstreamRequest{method: poll.Method, path: poll.Path, rawQuery: poll.RawQuery, headers: headers, body: poll.Body, internal: true})
	if bErr != nil {
		return nil, bErr
	}
	return &JobPollResponse{StatusCode: reply.status, ContentType: reply.contentType, Body: reply.body}, nil
}
