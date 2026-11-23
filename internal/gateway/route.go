// Package gateway derives "which consumer may call which model" from the
// Higress gateway's own configuration: per-model Ingresses (matched on the
// header that model-router fills from the request body) and the key-auth
// WasmPlugin. It mirrors what the gateway enforces; it enforces nothing.
package gateway

import (
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
)

// HigressAnnotationPrefix is the only prefix Higress accepts for its
// match-header annotations (pkg/ingress/kube/annotations/match.go doMatch).
const HigressAnnotationPrefix = "higress.io/"

// IngressClassAnnotation is the legacy ingress class annotation.
const IngressClassAnnotation = "kubernetes.io/ingress.class"

// Route is one per-model gateway route: an Ingress whose match requires the
// model header to equal Model.
type Route struct {
	// Name is the Envoy route name Higress gives the Ingress, which is what
	// key-auth matchRules[].ingress is compared against.
	Name string
	// Model is the exact model header value the route matches.
	Model string
	// Hosts the route answers on. Empty means any host.
	Hosts   []string
	Created time.Time
}

// RouteOptions controls how Ingresses are read.
type RouteOptions struct {
	// ModelHeader is the header model-router writes the body's model into
	// (model-router modelToHeader), e.g. x-higress-llm-model.
	ModelHeader string
	// IngressClass the gateway watches. Empty means every Ingress.
	IngressClass string
	// SystemNamespace is the gateway's namespace (higress-system). Ingresses
	// there get a route name without the namespace.
	SystemNamespace string
}

// RouteName returns the Envoy route name Higress uses for an Ingress
// (pkg/ingress/kube/common/tool.go GenerateUniqueRouteName).
func RouteName(namespace, name, systemNamespace string) string {
	if namespace == systemNamespace {
		return name
	}
	return namespace + "/" + name
}

// ParseIngress returns the per-model route an Ingress defines, if any.
// skipped is non-empty when the Ingress matches the model header in a way that
// cannot be enumerated (prefix or regex match); the caller logs it.
func ParseIngress(ing *networkingv1.Ingress, opts RouteOptions) (route *Route, skipped string) {
	if !classMatches(ing, opts.IngressClass) {
		return nil, ""
	}
	header := strings.ToLower(opts.ModelHeader)
	var model string
	var nonExact []string
	for k, v := range ing.Annotations {
		if !strings.HasPrefix(k, HigressAnnotationPrefix) {
			continue
		}
		rest := k[len(HigressAnnotationPrefix):]
		for _, kind := range []string{"exact", "prefix", "regex"} {
			p := kind + "-match-header-"
			if !strings.HasPrefix(rest, p) || strings.ToLower(rest[len(p):]) != header {
				continue
			}
			if kind == "exact" {
				// Used verbatim, as Higress does.
				model = v
			} else {
				nonExact = append(nonExact, kind)
			}
		}
	}
	if len(nonExact) > 0 {
		sort.Strings(nonExact)
		return nil, strings.Join(nonExact, "+") + " match on " + header + " cannot be listed"
	}
	if model == "" {
		return nil, ""
	}
	return &Route{
		Name:    RouteName(ing.Namespace, ing.Name, opts.SystemNamespace),
		Model:   model,
		Hosts:   ingressHosts(ing),
		Created: ing.CreationTimestamp.Time,
	}, ""
}

// nginxClass is the class Higress treats as "also take class-less Ingresses".
const nginxClass = "nginx"

// classMatches mirrors Higress shouldProcessIngressWithClass
// (pkg/ingress/kube/ingressv1/controller.go): the annotation wins over
// spec.ingressClassName, and class "nginx" also takes Ingresses without one.
func classMatches(ing *networkingv1.Ingress, class string) bool {
	if class == "" {
		return true
	}
	if a, ok := ing.Annotations[IngressClassAnnotation]; ok {
		if class == nginxClass {
			return a == "" || a == nginxClass
		}
		return a == class
	}
	name := ing.Spec.IngressClassName
	if class == nginxClass {
		return name == nil || *name == "" || *name == nginxClass
	}
	return name != nil && *name == class
}

// ingressHosts returns the hosts an Ingress serves, or nil when one of its
// rules (or a rule-less default backend) accepts any host.
func ingressHosts(ing *networkingv1.Ingress) []string {
	if len(ing.Spec.Rules) == 0 {
		return nil
	}
	set := map[string]struct{}{}
	for _, r := range ing.Spec.Rules {
		if r.Host == "" {
			return nil
		}
		set[strings.ToLower(r.Host)] = struct{}{}
	}
	hosts := make([]string, 0, len(set))
	for h := range set {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// ServesHost reports whether the route answers on host (port already stripped).
// A wildcard host ("*.example.com") follows Envoy virtual-host matching, which
// is what the gateway runs: a suffix match where "*" stands for one or more
// characters, so it also covers "a.b.example.com" (unlike the Ingress API's
// one-label wording).
func (r Route) ServesHost(host string) bool {
	if len(r.Hosts) == 0 {
		return true
	}
	host = strings.ToLower(host)
	for _, h := range r.Hosts {
		if h == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(h, "*"); ok {
			if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

// StripPort removes a :port suffix from a Host header value, IPv6 aware.
func StripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if i := strings.Index(host, "]"); i >= 0 {
			return host[:i+1]
		}
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		return host[:i]
	}
	return host
}
