package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    cliproxy_host_call_fn call;
    cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;
static int mini_host_call(cliproxy_host_api* api, const char* method, const uint8_t* in, size_t len, cliproxy_buffer* out) {
    return api->call(api->host_ctx, method, in, len, out);
}
static void mini_host_free(cliproxy_host_api* api, void* ptr, size_t len) {
    api->free_buffer(ptr, len);
}
extern int miniPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void miniPluginFree(void*, size_t);
extern void miniPluginShutdown(void);
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

const pluginID = "cpa-mini-usage"
const pluginVersion = "0.1.18"

var host *C.cliproxy_host_api
var initOnce sync.Once
var tracker = newEngine()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(h *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if p == nil {
		return 1
	}
	host = h
	p.abi_version = 1
	p.call = C.cliproxy_plugin_call_fn(C.miniPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.miniPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.miniPluginShutdown)
	return 0
}

//export miniPluginCall
func miniPluginCall(method *C.char, req *C.uint8_t, n C.size_t, resp *C.cliproxy_buffer) (rc C.int) {
	defer func() {
		if v := recover(); v != nil {
			writeBuffer(resp, errorEnvelope("internal_error", "plugin request failed"))
			rc = 0
		}
	}()
	if resp != nil {
		resp.ptr, resp.len = nil, 0
	}
	if method == nil || resp == nil {
		return 1
	}
	var raw []byte
	if req != nil && n > 0 {
		raw = C.GoBytes(unsafe.Pointer(req), C.int(n))
	}
	writeBuffer(resp, dispatch(C.GoString(method), raw))
	return 0
}

//export miniPluginFree
func miniPluginFree(p unsafe.Pointer, _ C.size_t) {
	if p != nil {
		C.free(p)
	}
}

//export miniPluginShutdown
func miniPluginShutdown() { tracker.shutdown() }

func writeBuffer(b *C.cliproxy_buffer, body []byte) {
	if b == nil {
		return
	}
	b.ptr, b.len = nil, 0
	if len(body) > 0 {
		b.ptr = C.CBytes(body)
		b.len = C.size_t(len(body))
	}
}

func hostRPC(name string, arg any) (any, error) {
	if host == nil || host.call == nil {
		return nil, fmt.Errorf("host callbacks unavailable")
	}
	data, err := json.Marshal(arg)
	if err != nil {
		return nil, err
	}
	method := C.CString(name)
	defer C.free(unsafe.Pointer(method))
	input := C.CBytes(data)
	defer C.free(input)
	var out C.cliproxy_buffer
	code := C.mini_host_call(host, method, (*C.uint8_t)(input), C.size_t(len(data)), &out)
	var raw []byte
	if out.ptr != nil {
		if out.len > 0 {
			raw = C.GoBytes(out.ptr, C.int(out.len))
		}
		if host.free_buffer != nil {
			C.mini_host_free(host, out.ptr, out.len)
		}
	}
	if code != 0 {
		return nil, fmt.Errorf("host callback %s failed (code %d)", name, int(code))
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid host response: %w", err)
	}
	if m, ok := v.(map[string]any); ok {
		if okv, present := m["ok"].(bool); present {
			if !okv {
				return nil, fmt.Errorf("host callback %s rejected request", name)
			}
			return m["result"], nil
		}
	}
	return v, nil
}

func dispatch(method string, body []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		initOnce.Do(func() { tracker.start() })
		return okEnvelope(map[string]any{
			"schema_version": 2,
			"metadata": map[string]string{
				"Name": "CPA Mini Usage", "Version": pluginVersion, "Author": "CPA Mini Usage Contributors",
				"GitHubRepository": "https://github.com/chilin11/cpa-mini-usage",
			},
			"capabilities": map[string]bool{"usage_plugin": true, "management_api": true},
		})
	case "management.register":
		return okEnvelope(map[string]any{
			"Routes": []map[string]string{
				{"Method": "GET", "Path": "/plugins/" + pluginID + "/data"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/refresh"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/refresh-account"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/settings"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/consume-reset"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/reset-credits"},
			},
			"Resources": []map[string]string{
				{"Path": "/dashboard", "Menu": "Mini Usage", "Description": "Quotas and credential usage"},
			},
		})
	case "management.handle":
		var req struct {
			Method string
			Path   string
			Body   []byte
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return errorEnvelope("bad_request", "invalid request")
		}
		if req.Method == "GET" && strings.HasSuffix(req.Path, "/dashboard") && strings.Contains(req.Path, "/resource/plugins/") {
			return okEnvelope(httpResponse(200, "text/html; charset=utf-8", []byte(dashboardHTML)))
		}
		if req.Method == "GET" && strings.HasSuffix(req.Path, "/"+pluginID+"/data") {
			tracker.syncAuthStatus(hostRPC)
			data, err := json.Marshal(tracker.snapshot())
			if err != nil {
				return okEnvelope(httpResponse(500, "application/json", []byte(`{"error":"snapshot_encoding_failed"}`)))
			}
			return okEnvelope(httpResponse(200, "application/json; charset=utf-8", data))
		}
		if req.Method == "POST" && strings.HasSuffix(req.Path, "/"+pluginID+"/refresh") {
			previous, accepted := tracker.kickFullRefresh()
			if !accepted {
				return okEnvelope(httpResponse(409, "application/json", []byte(`{"error":"refresh_busy"}`)))
			}
			body, _ := json.Marshal(map[string]any{"status": "refreshing", "quota_refresh_finished_at": previous})
			return okEnvelope(httpResponse(202, "application/json; charset=utf-8", body))
		}
		if req.Method == "POST" && strings.HasSuffix(req.Path, "/"+pluginID+"/refresh-account") {
			var input struct {
				AuthID string `json:"auth_id"`
			}
			if len(req.Body) > 4096 || json.Unmarshal(req.Body, &input) != nil {
				return okEnvelope(httpResponse(400, "application/json", []byte(`{"error":"invalid_auth_id"}`)))
			}
			status, previous, failure := tracker.kickAccountRefresh(input.AuthID)
			result := map[string]any{"status": "refreshing", "auth_id": input.AuthID, "refresh_finished_at": previous}
			if failure != "" {
				result = map[string]any{"error": failure}
			}
			body, _ := json.Marshal(result)
			return okEnvelope(httpResponse(status, "application/json; charset=utf-8", body))
		}
		if req.Method == "POST" && strings.HasSuffix(req.Path, "/"+pluginID+"/reset-credits") {
			var input struct {
				AuthID string `json:"auth_id"`
			}
			if len(req.Body) > 4096 || json.Unmarshal(req.Body, &input) != nil {
				return okEnvelope(httpResponse(400, "application/json", []byte(`{"error":"invalid_auth_id"}`)))
			}
			status, list, failure := tracker.listResetCredits(input.AuthID)
			var body []byte
			if failure != "" {
				body, _ = json.Marshal(map[string]any{"error": failure})
			} else {
				body, _ = json.Marshal(list)
			}
			return okEnvelope(httpResponse(status, "application/json; charset=utf-8", body))
		}
		if req.Method == "POST" && strings.HasSuffix(req.Path, "/"+pluginID+"/consume-reset") {
			var input struct {
				AuthID          string `json:"auth_id"`
				RedeemRequestID string `json:"redeem_request_id"`
				CreditID        string `json:"credit_id"`
			}
			if len(req.Body) > 4096 || json.Unmarshal(req.Body, &input) != nil {
				return okEnvelope(httpResponse(400, "application/json", []byte(`{"error":"invalid_auth_id"}`)))
			}
			status, result, failure := tracker.consumeReset(input.AuthID, input.RedeemRequestID, input.CreditID)
			var body []byte
			if failure != "" {
				body, _ = json.Marshal(map[string]any{"error": failure})
			} else {
				body, _ = json.Marshal(result)
			}
			return okEnvelope(httpResponse(status, "application/json; charset=utf-8", body))
		}
		if req.Method == "POST" && strings.HasSuffix(req.Path, "/"+pluginID+"/settings") {
			var input struct {
				PersistenceEnabled *bool `json:"persistence_enabled"`
			}
			if len(req.Body) > 4096 || json.Unmarshal(req.Body, &input) != nil || input.PersistenceEnabled == nil {
				return okEnvelope(httpResponse(400, "application/json", []byte(`{"error":"invalid_settings"}`)))
			}
			settings, err := tracker.setPersistence(*input.PersistenceEnabled)
			if err != nil {
				return okEnvelope(httpResponse(500, "application/json", []byte(`{"error":"storage_write_failed"}`)))
			}
			body, _ := json.Marshal(map[string]any{"persistence": settings})
			return okEnvelope(httpResponse(200, "application/json; charset=utf-8", body))
		}
		return okEnvelope(httpResponse(404, "application/json", []byte(`{"error":"not_found"}`)))
	case "usage.handle":
		tracker.capture(body)
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "unsupported plugin method")
	}
}

func httpResponse(status int, contentType string, body []byte) map[string]any {
	return map[string]any{
		"StatusCode": status,
		"Headers": map[string][]string{
			"Content-Type": {contentType}, "Cache-Control": {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		"Body": base64.StdEncoding.EncodeToString(body),
	}
}

func okEnvelope(v any) []byte {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
	return b
}
func errorEnvelope(code, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
	return b
}
