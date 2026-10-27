// Package agentgateway holds end-to-end tests for the Agent Gateway. They run
// against a deployed Bifrost whose config sets
// server.a2a_allow_private_push_callbacks to true, and skip when
// AGENTGATEWAY_URL is unset.
package agentgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/maximhq/bifrost/tests/agentgateway/driver"
	"github.com/maximhq/bifrost/tests/agentgateway/fixture"
)

// envOr returns an environment value or the fallback.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestLifecycle runs the complete Agent Gateway lifecycle and verifies the
// resulting Agent logs.
//
// Environment:
//
//	AGENTGATEWAY_URL         gateway base URL (required)
//	AGENTGATEWAY_ADMIN_TOKEN session token, or AGENTGATEWAY_ADMIN_USER/_PASS to log in
//	AGENTGATEWAY_VK          optional virtual key
//	AGENTGATEWAY_HOST        host the gateway uses to reach this process (default 127.0.0.1)
//	AGENTGATEWAY_BIND        local bind address for fixture and callback (default 127.0.0.1)
func TestLifecycle(t *testing.T) {
	testLifecycle(t, false)
}

func TestLifecycleWithoutUpstreamPushTokens(t *testing.T) {
	testLifecycle(t, true)
}

func testLifecycle(t *testing.T, omitPushTokens bool) {
	t.Helper()
	gwURL := os.Getenv("AGENTGATEWAY_URL")
	if gwURL == "" {
		t.Skip("AGENTGATEWAY_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	token := os.Getenv("AGENTGATEWAY_ADMIN_TOKEN")
	if user := os.Getenv("AGENTGATEWAY_ADMIN_USER"); token == "" && user != "" {
		var err error
		if token, err = driver.Login(ctx, gwURL, user, os.Getenv("AGENTGATEWAY_ADMIN_PASS")); err != nil {
			t.Fatal(err)
		}
	}
	gw := driver.NewGateway(gwURL, token)
	bind, host := envOr("AGENTGATEWAY_BIND", "127.0.0.1")+":0", envOr("AGENTGATEWAY_HOST", "127.0.0.1")

	start := fixture.Start
	if omitPushTokens {
		start = fixture.StartWithoutPushReadTokens
	}
	fx, err := start(bind, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fx.Close() })
	sink, err := driver.StartSink(bind, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	agent := fmt.Sprintf("lifecycle-fixture-%d", time.Now().UnixNano())
	if err := gw.RegisterAgent(ctx, agent, fx.CardURL, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := gw.DeleteAgent(c, agent); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	vk := os.Getenv("AGENTGATEWAY_VK")
	res, err := (&driver.Lifecycle{Gateway: gw, AgentName: agent, VirtualKey: vk, Sink: sink, Release: fx.ReleaseCheckpoint}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.AssertLogs(ctx, gw, agent, res, true, driver.Tags{}); err != nil {
		t.Fatal(err)
	}
}

// pushTenantAuth models independently authorized upstream tenants. It redacts
// read tokens without modifying the notification credential in the SDK store.
type pushTenantAuth struct {
	a2asrv.PassthroughCallInterceptor
	tenant    string
	omitToken bool
}

func (a pushTenantAuth) Before(ctx context.Context, call *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	values, _ := call.ServiceParams().Get("Authorization")
	if call.Tenant() != a.tenant || len(values) != 1 || values[0] != "Bearer "+a.tenant {
		return ctx, nil, a2a.ErrUnauthorized
	}
	call.User = a2asrv.NewAuthenticatedUser(a.tenant, nil)
	return ctx, nil, nil
}

func (a pushTenantAuth) After(_ context.Context, call *a2asrv.CallContext, response *a2asrv.Response) error {
	if !a.omitToken || response.Err != nil {
		return nil
	}
	switch call.Method() {
	case "GetTaskPushConfig":
		response.Payload.(*a2a.PushConfig).Token = ""
	case "ListTaskPushConfigs":
		for _, config := range response.Payload.(*a2a.ListTaskPushConfigResponse).Configs {
			config.Token = ""
		}
	}
	return nil
}

type pushTenantExecutor struct{}

func (pushTenantExecutor) Execute(_ context.Context, ctx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ctx.StoredTask == nil && !yield(a2a.NewSubmittedTask(ctx, ctx.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ctx, a2a.TaskStateInputRequired, nil), nil)
	}
}

func (pushTenantExecutor) Cancel(_ context.Context, ctx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ctx, a2a.TaskStateCanceled, nil), nil)
	}
}

type pushTenantCredentials struct {
	securityHeader
	tenant string
}

func (h pushTenantCredentials) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	req.ServiceParams.Append("Authorization", "Bearer "+h.tenant)
	return h.securityHeader.Before(ctx, req)
}

// TestPushTenantIsolation uses the real HTTP gateway, its config database and
// plugin/auth middleware, and SDK upstream task authorization in both bindings.
func TestPushTenantIsolation(t *testing.T) {
	gatewayURL := os.Getenv("AGENTGATEWAY_URL")
	if gatewayURL == "" {
		t.Skip("AGENTGATEWAY_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	token := os.Getenv("AGENTGATEWAY_ADMIN_TOKEN")
	if token == "" {
		var err error
		token, err = driver.Login(ctx, gatewayURL, os.Getenv("AGENTGATEWAY_ADMIN_USER"), os.Getenv("AGENTGATEWAY_ADMIN_PASS"))
		if err != nil {
			t.Fatal(err)
		}
	}
	gateway := driver.NewGateway(gatewayURL, token)
	for _, binding := range []a2a.TransportProtocol{a2a.TransportProtocolJSONRPC, a2a.TransportProtocolHTTPJSON} {
		for _, omitToken := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tokenless=%t", binding, omitToken), func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				sender := push.NewHTTPPushSender(&push.HTTPSenderConfig{AllowPrivateNetworks: true})
				upstream, pushStores := startPushTenantUpstream(t, ctx, binding, omitToken, sender)
				name := fmt.Sprintf("push-tenants-%d", time.Now().UnixNano())
				must(gateway.RegisterAgent(ctx, name, upstream.URL+"/card", false))
				t.Cleanup(func() { must(gateway.DeleteAgent(context.Background(), name)) })
				clients := map[string]*a2aclient.Client{}
				callbacks := map[string]*httptest.Server{}
				received := make(chan string, 32)
				for _, tenant := range []string{"tenant-a", "tenant-b"} {
					status, body, err := gateway.Do(ctx, http.MethodPost, "/api/governance/virtual-keys", struct {
						Name        string `json:"name"`
						IsActive    bool   `json:"is_active"`
						AgentGrants []struct {
							AgentName string `json:"agent_name"`
						} `json:"agent_grants"`
					}{name + "-" + tenant, true, []struct {
						AgentName string `json:"agent_name"`
					}{{name}}}, nil)
					must(err)
					if status != http.StatusOK {
						t.Fatalf("create virtual key: HTTP %d: %s", status, body)
					}
					var key struct {
						VirtualKey struct{ ID, Value string } `json:"virtual_key"`
					}
					must(json.Unmarshal(body, &key))
					if key.VirtualKey.Value == "" {
						t.Fatal("missing virtual key")
					}
					t.Cleanup(func() {
						status, body, err := gateway.Do(context.Background(), http.MethodDelete, "/api/governance/virtual-keys/"+key.VirtualKey.ID, nil, nil)
						must(err)
						if status != http.StatusOK {
							t.Errorf("delete virtual key: HTTP %d: %s", status, body)
						}
					})
					client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{a2a.NewAgentInterface(gatewayURL+"/agents/a2a/"+name+"/json-rpc", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(gateway.HTTP), a2aclient.WithCallInterceptors(pushTenantCredentials{securityHeader: securityHeader{key: key.VirtualKey.Value}, tenant: tenant}))
					must(err)
					clients[tenant] = client
					t.Cleanup(func() { _ = client.Destroy() })
					callbacks[tenant] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						body, err := io.ReadAll(req.Body)
						if err != nil || req.Header.Get("A2A-Notification-Token") != tenant || req.Header.Get("Authorization") != "Bearer callback-"+tenant || !bytes.Contains(body, []byte("shared-task")) {
							t.Errorf("wrong callback credentials or task for %s: %s", tenant, body)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						received <- tenant
						w.WriteHeader(http.StatusNoContent)
					}))
					t.Cleanup(callbacks[tenant].Close)
					_, err = client.CreateTaskPushConfig(ctx, &a2a.PushConfig{Tenant: tenant, TaskID: "shared-task", ID: "shared-config", URL: callbacks[tenant].URL, Token: tenant, Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "callback-" + tenant}})
					must(err)
				}
				for _, tenant := range []string{"tenant-a", "tenant-b"} {
					client := clients[tenant]
					got, err := client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: tenant, TaskID: "shared-task", ID: "shared-config"})
					must(err)
					if got.URL != callbacks[tenant].URL || got.Token != "" || got.Auth.Credentials != "" {
						t.Fatalf("wrong or unredacted callback: %+v", got)
					}
					listed, err := client.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{Tenant: tenant, TaskID: "shared-task"})
					must(err)
					if len(listed) != 1 || listed[0].URL != got.URL {
						t.Fatalf("wrong tenant list: %+v", listed)
					}
					// Both send paths must persist the legitimate embedded callback
					// under the same namespace the upstream authorizes.
					for _, streaming := range []bool{false, true} {
						id := "shared-config" // Updating the same config must replace, not fail binding.
						req := &a2a.SendMessageRequest{Tenant: tenant,
							Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
							Config:  &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{ID: id, URL: callbacks[tenant].URL, Token: tenant, Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "callback-" + tenant}}},
						}
						req.Message.TaskID = "shared-task"
						if streaming {
							for _, err := range client.SendStreamingMessage(ctx, req) {
								must(err)
							}
						} else {
							_, err := client.SendMessage(ctx, req)
							must(err)
						}
						embedded, err := client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: tenant, TaskID: "shared-task", ID: id})
						must(err)
						if embedded.URL != got.URL || embedded.Token != "" {
							t.Fatalf("wrong embedded callback: %+v", embedded)
						}
					}
					config, err := pushStores[tenant].Get(ctx, "shared-task", "shared-config")
					must(err)
					if config.URL == callbacks[tenant].URL || config.Token == tenant || config.Auth != nil {
						t.Fatal("downstream credentials reached upstream")
					}
					must(sender.SendPush(ctx, config, &a2a.Task{ID: "shared-task", ContextID: "context", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}))
				}
				seen := map[string]int{}
				for len(seen) < 2 {
					select {
					case tenant := <-received:
						seen[tenant]++
					case <-ctx.Done():
						t.Fatal("tenant callbacks were not delivered")
					}
				}
				for _, operation := range []string{"create", "get", "list", "delete", "embedded", "streaming-embedded"} {
					var err error
					client := clients["tenant-b"]
					switch operation {
					case "create":
						_, err = client.CreateTaskPushConfig(ctx, &a2a.PushConfig{Tenant: "tenant-a", TaskID: "shared-task", ID: "shared-config", URL: callbacks["tenant-b"].URL})
					case "get":
						_, err = client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: "tenant-a", TaskID: "shared-task", ID: "shared-config"})
					case "list":
						_, err = client.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{Tenant: "tenant-a", TaskID: "shared-task"})
					case "delete":
						err = client.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{Tenant: "tenant-a", TaskID: "shared-task", ID: "shared-config"})
					default:
						req := &a2a.SendMessageRequest{Tenant: "tenant-a", Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")), Config: &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{ID: "shared-config", URL: callbacks["tenant-b"].URL}}}
						req.Message.TaskID = "shared-task"
						if operation == "embedded" {
							_, err = client.SendMessage(ctx, req)
						} else {
							for _, streamErr := range client.SendStreamingMessage(ctx, req) {
								if streamErr != nil {
									err = streamErr
								}
							}
						}
					}
					if !errors.Is(err, a2a.ErrUnauthorized) {
						t.Fatalf("%s crossed tenant ownership: %v", operation, err)
					}
				}
				_, err := clients["tenant-a"].CreateTaskPushConfig(ctx, &a2a.PushConfig{Tenant: "tenant-a", TaskID: "only-a", ID: "config", URL: callbacks["tenant-a"].URL})
				must(err)
				_, err = clients["tenant-b"].GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: "tenant-b", TaskID: "only-a", ID: "config"})
				if !errors.Is(err, a2a.ErrTaskNotFound) {
					t.Fatalf("noncolliding task leaked: %v", err)
				}
				must(clients["tenant-a"].DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{Tenant: "tenant-a", TaskID: "shared-task", ID: "shared-config"}))
				got, err := clients["tenant-b"].GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: "tenant-b", TaskID: "shared-task", ID: "shared-config"})
				must(err)
				if got.URL != callbacks["tenant-b"].URL {
					t.Fatal("tenant-a deletion affected tenant-b")
				}
			})
		}
	}
}

// startPushTenantUpstream gives each authenticated tenant independent SDK task
// and push stores, with deliberately colliding task IDs.
func startPushTenantUpstream(t *testing.T, ctx context.Context, binding a2a.TransportProtocol, omitToken bool, sender *push.HTTPPushSender) (*httptest.Server, map[string]*push.InMemoryPushConfigStore) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	card := &a2a.AgentCard{
		Name: "tenant-push-fixture", Version: "1", DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{PushNotifications: true, Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(upstream.URL+"/rpc", binding)},
	}
	if binding == a2a.TransportProtocolHTTPJSON {
		card.SupportedInterfaces[0].URL = upstream.URL + "/rest"
	}
	mux.Handle("/card", a2asrv.NewStaticAgentCardHandler(card))
	pushStores := map[string]*push.InMemoryPushConfigStore{}
	protocols := map[string]http.Handler{}
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		tasks := taskstore.NewInMemory(nil)
		_, err := tasks.Create(ctx, &a2a.Task{ID: "shared-task", ContextID: "context", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}})
		must(err)
		if tenant == "tenant-a" {
			_, err = tasks.Create(ctx, &a2a.Task{ID: "only-a", ContextID: "context", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}})
			must(err)
		}
		pushStores[tenant] = push.NewInMemoryStore()
		handler := a2asrv.NewHandler(pushTenantExecutor{}, a2asrv.WithTaskStore(tasks), a2asrv.WithPushNotifications(pushStores[tenant], sender), a2asrv.WithCallInterceptors(pushTenantAuth{tenant: tenant, omitToken: omitToken}))
		protocols[tenant] = a2asrv.NewJSONRPCHandler(handler)
		if binding == a2a.TransportProtocolHTTPJSON {
			protocols[tenant] = http.StripPrefix("/rest", a2asrv.NewTenantRESTHandler("/{*}", handler))
		}
	}
	serve := func(w http.ResponseWriter, req *http.Request) {
		handler := protocols[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
		if handler == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, req)
	}
	mux.HandleFunc("/rpc", serve)
	mux.HandleFunc("/rest/", serve)
	return upstream, pushStores
}
