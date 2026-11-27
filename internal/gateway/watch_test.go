package gateway

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func wasmPlugin(t *testing.T, ns, name, url, specYAML string) *unstructured.Unstructured {
	t.Helper()
	var spec map[string]any
	if err := yaml.Unmarshal([]byte(specYAML), &spec); err != nil {
		t.Fatal(err)
	}
	spec["url"] = url
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion("extensions.higress.io/v1alpha1")
	u.SetKind("WasmPlugin")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

const keyAuthURL = "oci://registry.example.com/plugins/key-auth:1.0.0"

type harness struct {
	w      *Watcher
	kube   *kubefake.Clientset
	dyn    *dynfake.FakeDynamicClient
	cancel context.CancelFunc
}

func startWatcher(t *testing.T, keyAuthPlugin string, plugins ...*unstructured.Unstructured) *harness {
	t.Helper()
	kube := kubefake.NewSimpleClientset(
		modelIngress("higress-system", "qwen", "qwen-72b"),
		modelIngress("llm", "llama", "llama-3-70b"),
	)
	objs := make([]runtime.Object, len(plugins))
	for i, p := range plugins {
		objs[i] = p
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{WasmPluginGVR: "WasmPluginList"}, objs...)
	w := NewWatcher(kube, dyn, WatchOptions{RouteOptions: opts, KeyAuthPlugin: keyAuthPlugin},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	waitFor(t, "ready", w.Ready)
	return &harness{w, kube, dyn, cancel}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) list(consumer string) []string {
	return ids(h.w.Catalog().Models(consumer, "api.example.com"))
}

const watchSpec = `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [alice]}
- ingress: [llm/llama]
  config: {allow: [bob]}
`

func TestWatcherAutoDetectAndLiveUpdate(t *testing.T) {
	h := startWatcher(t, "",
		wasmPlugin(t, "higress-system", "key-auth-1.0.0", keyAuthURL, watchSpec),
		wasmPlugin(t, "higress-system", "model-router", "oci://registry.example.com/plugins/model-router:1.0.0", `{}`),
	)
	if got := h.list("alice"); !reflect.DeepEqual(got, []string{"qwen-72b"}) {
		t.Fatalf("alice: %v", got)
	}

	// allow edit: alice gains llama, without restart.
	u, err := h.dyn.Resource(WasmPluginGVR).Namespace("higress-system").Get(context.Background(), "key-auth-1.0.0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "matchRules")
	rules[1].(map[string]any)["config"] = map[string]any{"allow": []any{"bob", "alice"}}
	_ = unstructured.SetNestedSlice(u.Object, rules, "spec", "matchRules")
	if _, err := h.dyn.Resource(WasmPluginGVR).Namespace("higress-system").Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice to see llama", func() bool {
		return reflect.DeepEqual(h.list("alice"), []string{"llama-3-70b", "qwen-72b"})
	})

	// New model route appears; a deleted one disappears.
	_, err = h.kube.NetworkingV1().Ingresses("higress-system").Create(context.Background(),
		modelIngress("higress-system", "ds", "deepseek-v3"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.kube.NetworkingV1().Ingresses("llm").Delete(context.Background(), "llama", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// ds has no rule and global_auth is unset: open, listed for nobody.
	waitFor(t, "llama gone", func() bool { return reflect.DeepEqual(h.list("alice"), []string{"qwen-72b"}) })
	if got := h.w.Catalog().OpenModelRoutes(); len(got) != 1 || got[0].Route != "ds" {
		t.Errorf("open routes: %+v", got)
	}
}

func TestWatcherPluginSelection(t *testing.T) {
	second := wasmPlugin(t, "higress-system", "key-auth-b", "oci://registry.example.com/plugins/key_auth@sha256:abc", `
defaultConfig:
  consumers: [{name: zed, credential: kz}]
  keys: [k]
`)
	t.Run("several candidates: nothing listed", func(t *testing.T) {
		h := startWatcher(t, "", wasmPlugin(t, "higress-system", "key-auth-a", keyAuthURL, watchSpec), second)
		if !h.w.Catalog().KeyAuth.Invalid || len(h.list("alice")) != 0 {
			t.Errorf("want invalid, got %+v", h.w.Catalog().KeyAuth)
		}
	})
	t.Run("explicit flag picks one", func(t *testing.T) {
		h := startWatcher(t, "higress-system/key-auth-b", wasmPlugin(t, "higress-system", "key-auth-a", keyAuthURL, watchSpec), second)
		if got := h.list("zed"); !reflect.DeepEqual(got, []string{"llama-3-70b", "qwen-72b"}) {
			t.Errorf("zed: %v", got)
		}
	})
	t.Run("none: nothing listed", func(t *testing.T) {
		h := startWatcher(t, "")
		if len(h.list("alice")) != 0 {
			t.Errorf("alice: %v", h.list("alice"))
		}
	})
	t.Run("plugin outside the watched namespace is not seen", func(t *testing.T) {
		h := startWatcher(t, "", wasmPlugin(t, "other", "key-auth", keyAuthURL, watchSpec))
		if len(h.list("alice")) != 0 {
			t.Errorf("alice: %v", h.list("alice"))
		}
	})
}
