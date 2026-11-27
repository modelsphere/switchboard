package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// WasmPluginGVR is the Higress WasmPlugin resource.
var WasmPluginGVR = schema.GroupVersionResource{
	Group: "extensions.higress.io", Version: "v1alpha1", Resource: "wasmplugins",
}

// WatchOptions configures the watcher.
type WatchOptions struct {
	RouteOptions
	// KeyAuthPlugin is "namespace/name" of the key-auth WasmPlugin. Empty
	// selects the single WasmPlugin in SystemNamespace whose url names
	// key-auth (key-auth / key_auth).
	KeyAuthPlugin string
	// Resync is the informer resync period. The catalog is rebuilt from the
	// informer store on every event, so 0 (no resync) is the right default.
	Resync time.Duration
}

// ValidateKeyAuthPlugin checks the --key-auth-plugin value.
func ValidateKeyAuthPlugin(v string) error {
	if v == "" {
		return nil
	}
	ns, name, ok := strings.Cut(v, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("--key-auth-plugin %q: want namespace/name", v)
	}
	return nil
}

// Watcher keeps a Catalog current from informers on Ingresses and WasmPlugins.
type Watcher struct {
	opts    WatchOptions
	log     *slog.Logger
	catalog atomic.Pointer[Catalog]
	ready   atomic.Bool

	ingInf cache.SharedIndexInformer
	wpInf  cache.SharedIndexInformer

	mu       sync.Mutex
	lastWarn string
}

// NewWatcher wires informers. Ingresses are watched cluster-wide (Higress
// does the same by default); WasmPlugins only in SystemNamespace.
func NewWatcher(kube kubernetes.Interface, dyn dynamic.Interface, opts WatchOptions, log *slog.Logger) *Watcher {
	w := &Watcher{opts: opts, log: log}
	w.catalog.Store(&Catalog{})

	kf := informers.NewSharedInformerFactory(kube, opts.Resync)
	w.ingInf = kf.Networking().V1().Ingresses().Informer()

	pluginNS := opts.SystemNamespace
	if ns, _, ok := strings.Cut(opts.KeyAuthPlugin, "/"); ok {
		pluginNS = ns // format checked by ValidateKeyAuthPlugin
	}
	df := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, opts.Resync, pluginNS, nil)
	w.wpInf = df.ForResource(WasmPluginGVR).Informer()

	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { w.rebuild() },
		UpdateFunc: func(any, any) { w.rebuild() },
		DeleteFunc: func(any) { w.rebuild() },
	}
	_, _ = w.ingInf.AddEventHandler(h)
	_, _ = w.wpInf.AddEventHandler(h)
	return w
}

// Run starts the informers and blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	go w.ingInf.Run(ctx.Done())
	go w.wpInf.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), w.ingInf.HasSynced, w.wpInf.HasSynced) {
		if ctx.Err() != nil {
			return nil // shut down before the first sync
		}
		return fmt.Errorf("informer caches did not sync")
	}
	w.rebuild()
	w.ready.Store(true)
	w.log.Info("gateway config synced")
	<-ctx.Done()
	return nil
}

// Ready reports whether the first full sync completed.
func (w *Watcher) Ready() bool { return w.ready.Load() }

// Catalog returns the current snapshot.
func (w *Watcher) Catalog() *Catalog { return w.catalog.Load() }

func (w *Watcher) rebuild() {
	if !w.ingInf.HasSynced() || !w.wpInf.HasSynced() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	var warnings []string
	var routes []Route
	for _, obj := range w.ingInf.GetStore().List() {
		ing, ok := obj.(*networkingv1.Ingress)
		if !ok {
			continue
		}
		r, skipped := ParseIngress(ing, w.opts.RouteOptions)
		if skipped != "" {
			warnings = append(warnings, fmt.Sprintf("ingress %s/%s: %s", ing.Namespace, ing.Name, skipped))
		}
		if r != nil {
			routes = append(routes, *r)
		}
	}
	slices.SortFunc(routes, func(a, b Route) int { return strings.Compare(a.Name, b.Name) })

	ka, kaWarn := w.keyAuth()
	warnings = append(warnings, kaWarn...)
	c := &Catalog{Routes: routes, KeyAuth: ka}
	for _, o := range c.OpenModelRoutes() {
		warnings = append(warnings, fmt.Sprintf("model route %s (model %q) is not authenticated by key-auth %s; not listed there", o.Route, o.Model, o.Where))
	}
	w.catalog.Store(c)

	// Log warnings when they change, not on every event. The store lists in
	// random order, so sort before comparing.
	slices.Sort(warnings)
	joined := strings.Join(warnings, "\n")
	if joined != w.lastWarn {
		w.lastWarn = joined
		for _, s := range warnings {
			w.log.Warn(s)
		}
	}
	w.log.Debug("catalog rebuilt", "routes", len(routes))
}

func (w *Watcher) keyAuth() (KeyAuth, []string) {
	var candidates []*unstructured.Unstructured
	for _, obj := range w.wpInf.GetStore().List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if w.opts.KeyAuthPlugin != "" {
			if u.GetNamespace()+"/"+u.GetName() == w.opts.KeyAuthPlugin {
				candidates = append(candidates, u)
			}
			continue
		}
		url, _, _ := unstructured.NestedString(u.Object, "spec", "url")
		if isKeyAuthURL(url) {
			candidates = append(candidates, u)
		}
	}
	switch len(candidates) {
	case 0:
		return KeyAuth{}, []string{"no key-auth WasmPlugin found; every model route counts as unauthenticated"}
	case 1:
	default:
		names := make([]string, len(candidates))
		for i, c := range candidates {
			names[i] = c.GetNamespace() + "/" + c.GetName()
		}
		return KeyAuth{Invalid: true}, []string{"several key-auth WasmPlugins (" + strings.Join(names, ", ") + "); set --key-auth-plugin"}
	}
	spec, _, _ := unstructured.NestedMap(candidates[0].Object, "spec")
	return ParseKeyAuth(spec)
}

func isKeyAuthURL(url string) bool {
	last := url[strings.LastIndex(url, "/")+1:]
	last, _, _ = strings.Cut(last, ":")
	last, _, _ = strings.Cut(last, "@")
	last = strings.TrimSuffix(last, ".wasm")
	return last == "key-auth" || last == "key_auth"
}
