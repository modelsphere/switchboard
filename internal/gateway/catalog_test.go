package gateway

import (
	"reflect"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

var opts = RouteOptions{
	ModelHeader:     "x-higress-llm-model",
	IngressClass:    "higress",
	SystemNamespace: "higress-system",
}

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

type ingOpt func(*networkingv1.Ingress)

func hosts(h ...string) ingOpt {
	return func(i *networkingv1.Ingress) {
		i.Spec.Rules = nil
		for _, x := range h {
			i.Spec.Rules = append(i.Spec.Rules, networkingv1.IngressRule{Host: x})
		}
	}
}

func created(d time.Duration) ingOpt {
	return func(i *networkingv1.Ingress) { i.CreationTimestamp = metav1.NewTime(t0.Add(d)) }
}

func annotations(kv ...string) ingOpt {
	return func(i *networkingv1.Ingress) {
		for j := 0; j+1 < len(kv); j += 2 {
			i.Annotations[kv[j]] = kv[j+1]
		}
	}
}

func class(c string) ingOpt {
	return func(i *networkingv1.Ingress) { i.Spec.IngressClassName = &c }
}

// modelIngress is an Ingress routing model on the model header, the way the
// per-model routes are written for Higress.
func modelIngress(ns, name, model string, o ...ingOpt) *networkingv1.Ingress {
	c := "higress"
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			CreationTimestamp: metav1.NewTime(t0),
			Annotations:       map[string]string{},
		},
		Spec: networkingv1.IngressSpec{IngressClassName: &c},
	}
	if model != "" {
		ing.Annotations["higress.io/exact-match-header-x-higress-llm-model"] = model
	}
	for _, f := range o {
		f(ing)
	}
	return ing
}

func keyAuth(t *testing.T, specYAML string) (KeyAuth, []string) {
	t.Helper()
	var spec map[string]any
	if err := yaml.Unmarshal([]byte(specYAML), &spec); err != nil {
		t.Fatal(err)
	}
	return ParseKeyAuth(spec)
}

func catalog(t *testing.T, specYAML string, ings ...*networkingv1.Ingress) *Catalog {
	t.Helper()
	c := &Catalog{}
	for _, ing := range ings {
		if r, _ := ParseIngress(ing, opts); r != nil {
			c.Routes = append(c.Routes, *r)
		}
	}
	c.KeyAuth, _ = keyAuth(t, specYAML)
	return c
}

func ids(ms []Model) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

const consumersABC = `
  consumers:
  - {name: alice, credential: ka}
  - {name: bob, credential: kb}
  - {name: carol, credential: kc}
  keys: [Authorization]
`

func TestModels(t *testing.T) {
	routes := []*networkingv1.Ingress{
		modelIngress("higress-system", "qwen", "qwen-72b"),
		modelIngress("llm", "llama", "llama-3-70b"),
		modelIngress("llm", "ds", "deepseek-v3"),
	}
	tests := []struct {
		name string
		spec string
		want map[string][]string // consumer -> models
	}{
		{
			name: "allow per route; route name without namespace in system namespace",
			spec: `
defaultConfig:
  global_auth: false` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [alice]}
- ingress: [llm/llama]
  config: {allow: [bob]}
- ingress: [llm/ds]
  config: {allow: [alice, bob]}
`,
			want: map[string][]string{
				"alice": {"deepseek-v3", "qwen-72b"},
				"bob":   {"deepseek-v3", "llama-3-70b"},
				"carol": {},
			},
		},
		{
			name: "one rule naming several routes",
			spec: `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen, llm/llama, llm/ds]
  config: {allow: [carol]}
`,
			want: map[string][]string{
				"alice": {},
				"carol": {"deepseek-v3", "llama-3-70b", "qwen-72b"},
			},
		},
		{
			name: "rule without allow is an empty allow set: nobody",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {}
- ingress: [llm/llama]
  config: {allow: []}
`,
			want: map[string][]string{
				"alice": {"deepseek-v3"},
				"bob":   {"deepseek-v3"},
			},
		},
		{
			name: "global_auth unset with rules: unmatched routes are open, listed for nobody",
			spec: `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [alice]}
`,
			want: map[string][]string{"alice": {"qwen-72b"}, "bob": {}},
		},
		{
			name: "global_auth true: unmatched routes allow every consumer",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [alice]}
`,
			want: map[string][]string{
				"alice": {"deepseek-v3", "llama-3-70b", "qwen-72b"},
				"bob":   {"deepseek-v3", "llama-3-70b"},
			},
		},
		{
			name: "no rules, global_auth unset: global for everyone",
			spec: `
defaultConfig:` + consumersABC,
			want: map[string][]string{"carol": {"deepseek-v3", "llama-3-70b", "qwen-72b"}},
		},
		{
			name: "no rules, global_auth false: nothing authenticated",
			spec: `
defaultConfig:
  global_auth: false` + consumersABC,
			want: map[string][]string{"alice": {}},
		},
		{
			name: "allowed name that is not a configured consumer is ignored",
			spec: `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [mallory, alice]}
`,
			want: map[string][]string{"alice": {"qwen-72b"}, "mallory": {}},
		},
		{
			name: "rule with its own consumers replaces global config and ignores allow",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- ingress: [qwen]
  config:
    allow: [alice]
    consumers: [{name: dave, credential: kd}]
    keys: [Authorization]
`,
			want: map[string][]string{
				"alice": {"deepseek-v3", "llama-3-70b"},
				"dave":  {"qwen-72b"},
			},
		},
		{
			name: "configDisable drops the rule; _disable_ opens the route",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- ingress: [qwen]
  configDisable: true
  config: {allow: []}
- ingress: [llm/llama]
  config: {_disable_: true}
`,
			want: map[string][]string{"bob": {"deepseek-v3", "qwen-72b"}},
		},
		{
			name: "defaultConfigDisable: no global consumers",
			spec: `
defaultConfigDisable: true
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [alice]}
`,
			want: map[string][]string{"alice": {}},
		},
		{
			name: "rule without match condition invalidates the plugin",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- config: {allow: [alice]}
`,
			want: map[string][]string{"alice": {}},
		},
		{
			name: "rule with unparsable local config and no allow invalidates the plugin",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {keys: [x-api-key]}
`,
			want: map[string][]string{"alice": {}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := catalog(t, tt.spec, routes...)
			for consumer, want := range tt.want {
				got := ids(c.Models(consumer, "api.example.com"))
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: got %v, want %v", consumer, got, want)
				}
			}
		})
	}
}

func TestModelsFirstMatchingRuleWins(t *testing.T) {
	spec := `
defaultConfig:` + consumersABC + `
matchRules:
- domain: ["*.example.com"]
  config: {allow: [alice]}
- ingress: [qwen]
  config: {allow: [bob]}
`
	c := catalog(t, spec, modelIngress("higress-system", "qwen", "qwen-72b"))
	if got := ids(c.Models("alice", "api.example.com:443")); !reflect.DeepEqual(got, []string{"qwen-72b"}) {
		t.Errorf("alice on example.com: %v", got)
	}
	if got := ids(c.Models("bob", "api.example.com")); len(got) != 0 {
		t.Errorf("bob on example.com: %v (domain rule comes first)", got)
	}
	if got := ids(c.Models("bob", "api.other.org")); !reflect.DeepEqual(got, []string{"qwen-72b"}) {
		t.Errorf("bob on other.org: %v", got)
	}
}

func TestModelsServiceRuleNeverMatches(t *testing.T) {
	spec := `
defaultConfig:
  global_auth: true` + consumersABC + `
matchRules:
- service: [qwen.llm.svc.cluster.local]
  config: {allow: [alice]}
`
	c := catalog(t, spec, modelIngress("higress-system", "qwen", "qwen-72b"))
	ka, warn := keyAuth(t, spec)
	if len(warn) == 0 || !strings.Contains(warn[0], "service") {
		t.Errorf("want a service warning, got %v", warn)
	}
	if _, ok := ka.Allowed("qwen", "x"); !ok {
		t.Errorf("global_auth should still authenticate")
	}
	if got := ids(c.Models("bob", "x")); !reflect.DeepEqual(got, []string{"qwen-72b"}) {
		t.Errorf("bob: %v", got)
	}
}

func TestModelsUnionAcrossRoutes(t *testing.T) {
	spec := `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [llm/qwen-a]
  config: {allow: [alice]}
- ingress: [llm/qwen-b]
  config: {allow: [bob]}
`
	c := catalog(t, spec,
		modelIngress("llm", "qwen-a", "qwen-72b", created(2*time.Hour)),
		modelIngress("llm", "qwen-b", "qwen-72b", created(time.Hour)),
	)
	for _, who := range []string{"alice", "bob"} {
		ms := c.Models(who, "h")
		if len(ms) != 1 || ms[0].ID != "qwen-72b" {
			t.Fatalf("%s: %v", who, ms)
		}
	}
	if got := c.Models("bob", "h")[0].Created; !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("created = %v", got)
	}
	// alice can only use the route created later.
	if got := c.Models("alice", "h")[0].Created; !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("alice created = %v", got)
	}
}

func TestModelsIngressHosts(t *testing.T) {
	spec := `defaultConfig:` + consumersABC
	c := catalog(t, spec,
		modelIngress("llm", "a", "model-a", hosts("api.example.com")),
		modelIngress("llm", "b", "model-b", hosts("*.example.org")),
		modelIngress("llm", "c", "model-c"),
	)
	cases := map[string][]string{
		"api.example.com":      {"model-a", "model-c"},
		"API.example.com:8443": {"model-a", "model-c"},
		"x.example.org":        {"model-b", "model-c"},
		"a.b.example.org":      {"model-b", "model-c"}, // Envoy suffix wildcard
		".example.org":         {"model-c"},            // "*" needs at least one character
		"example.org":          {"model-c"},
	}
	for host, want := range cases {
		if got := ids(c.Models("alice", host)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", host, got, want)
		}
	}
}

func TestModelsNoConsumer(t *testing.T) {
	c := catalog(t, `defaultConfig:`+consumersABC, modelIngress("llm", "a", "m"))
	if got := c.Models("", "h"); len(got) != 0 {
		t.Errorf("empty consumer: %v", got)
	}
}

func TestParseIngress(t *testing.T) {
	tests := []struct {
		name      string
		ing       *networkingv1.Ingress
		wantModel string
		wantRoute string
		skipped   bool
	}{
		{"exact header", modelIngress("llm", "a", "m1"), "m1", "llm/a", false},
		{"system namespace", modelIngress("higress-system", "a", "m1"), "m1", "a", false},
		{"no header match", modelIngress("llm", "a", ""), "", "", false},
		{"other ingress class", modelIngress("llm", "a", "m1", class("nginx")), "", "", false},
		{"legacy class annotation", func() *networkingv1.Ingress {
			i := modelIngress("llm", "a", "m1")
			i.Spec.IngressClassName = nil
			i.Annotations[IngressClassAnnotation] = "higress"
			return i
		}(), "m1", "llm/a", false},
		{"other header", modelIngress("llm", "a", "", annotations("higress.io/exact-match-header-x-tenant", "t1")), "", "", false},
		{"header name case-insensitive", modelIngress("llm", "a", "", annotations("higress.io/exact-match-header-X-Higress-LLM-Model", "m2")), "m2", "llm/a", false},
		{"nginx prefix not honoured by higress", modelIngress("llm", "a", "", annotations("nginx.ingress.kubernetes.io/exact-match-header-x-higress-llm-model", "m3")), "", "", false},
		{"prefix match skipped", modelIngress("llm", "a", "", annotations("higress.io/prefix-match-header-x-higress-llm-model", "qwen-")), "", "", true},
		{"regex match skipped", modelIngress("llm", "a", "", annotations("higress.io/regex-match-header-x-higress-llm-model", "qwen.*")), "", "", true},
		{"model id with slash", modelIngress("llm", "a", "Qwen/Qwen3-8B"), "Qwen/Qwen3-8B", "llm/a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, skipped := ParseIngress(tt.ing, opts)
			if (skipped != "") != tt.skipped {
				t.Errorf("skipped = %q", skipped)
			}
			if tt.wantModel == "" {
				if r != nil {
					t.Errorf("want no route, got %+v", *r)
				}
				return
			}
			if r == nil || r.Model != tt.wantModel || r.Name != tt.wantRoute {
				t.Errorf("got %+v", r)
			}
		})
	}
}

func TestOpenModelRoutes(t *testing.T) {
	spec := `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [llm/a]
  config: {allow: [alice]}
`
	c := catalog(t, spec, modelIngress("llm", "a", "m1"), modelIngress("llm", "b", "m2"))
	want := []OpenRoute{{"llm/b", "m2", "for hosts no key-auth domain rule matches"}}
	if got := c.OpenModelRoutes(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestIsKeyAuthURL(t *testing.T) {
	for url, want := range map[string]bool{
		"oci://higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/key-auth:1.0.0": true,
		"oci://registry.example.com/plugins/key_auth@sha256:abc":                    true,
		"oci://registry.example.com/plugins/key-auth":                               true,
		"oci://registry.example.com/plugins/basic-auth:1.0.0":                       false,
		"oci://registry.example.com/key-auth/model-router:1.0.0":                    false,
	} {
		if got := isKeyAuthURL(url); got != want {
			t.Errorf("%s: %v", url, got)
		}
	}
}
