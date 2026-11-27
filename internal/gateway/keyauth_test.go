package gateway

import (
	"reflect"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

// Cases for the parts of the key-auth mirror that TestModels does not reach:
// the controller's CR → plugin JSON conversion and the plugin's own
// accept/reject decisions.
func TestKeyAuthMirror(t *testing.T) {
	routes := []*networkingv1.Ingress{
		modelIngress("higress-system", "qwen", "qwen-72b"),
		modelIngress("llm", "llama", "llama-3-70b"),
	}
	tests := []struct {
		name        string
		spec        string
		want        map[string][]string
		wantWarning string
	}{
		{
			name: "rule-local consumers with global_auth false: route open",
			spec: `
defaultConfig:
  global_auth: false` + consumersABC + `
matchRules:
- ingress: [qwen]
  config:
    consumers: [{name: dave, credential: kd}]
    keys: [Authorization]
`,
			want: map[string][]string{"dave": {}, "alice": {}},
		},
		{
			name: "pluginConfig is used verbatim; defaultConfig and matchRules ignored",
			spec: `
defaultConfig:
  global_auth: true` + consumersABC + `
pluginConfig:
  consumers: [{name: erin, credential: ke}]
  keys: [Authorization]
  _rules_:
  - _match_route_prefix_: [llm/]
    allow: [erin]
`,
			want: map[string][]string{"erin": {"llama-3-70b"}, "alice": {}},
		},
		{
			name: "defaultConfigDisable and no rules: plugin not applied",
			spec: `
defaultConfigDisable: true
defaultConfig:
  global_auth: true` + consumersABC,
			want:        map[string][]string{"alice": {}},
			wantWarning: "no config to apply",
		},
		{
			name: "invalid instance config with rules is tolerated: no global consumers",
			spec: `
defaultConfig:
  global_auth: true
  consumers:
  - {name: alice}
  keys: [Authorization]
matchRules:
- ingress: [qwen]
  config: {allow: [alice]}
`,
			want:        map[string][]string{"alice": {}},
			wantWarning: "no credential",
		},
		{
			name: "invalid instance config without rules rejects the plugin",
			spec: `
defaultConfig:
  global_auth: true
  consumers:
  - {name: alice, credential: k1}
  - {name: bob, credential: k1}
  keys: [Authorization]
`,
			want:        map[string][]string{"alice": {}, "bob": {}},
			wantWarning: "duplicate consumer credential",
		},
		{
			name: "consumer with both credential and credentials is rejected",
			spec: `
defaultConfig:
  consumers:
  - {name: alice, credential: k1, credentials: [k2]}
  keys: [Authorization]
`,
			want:        map[string][]string{"alice": {}},
			wantWarning: "both credential and credentials",
		},
		{
			name: "consumer with credentials list and own keys",
			spec: `
defaultConfig:
  consumers:
  - {name: alice, credentials: [k1, k2], keys: [x-api-key]}
`,
			want: map[string][]string{"alice": {"llama-3-70b", "qwen-72b"}},
		},
		{
			name: "in_query and in_header both false rejects the plugin",
			spec: `
defaultConfig:
  consumers: [{name: alice, credential: k1}]
  keys: [Authorization]
  in_query: false
  in_header: false
`,
			want:        map[string][]string{"alice": {}},
			wantWarning: "both false",
		},
		{
			name: "anonymous credentials: authenticated, but no consumer to list for",
			spec: `
defaultConfig:
  credentials: [k1]
  keys: [Authorization]
`,
			want: map[string][]string{"alice": {}},
		},
		{
			name: "allow plus an unparsable extra key: falls back to allow",
			spec: `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen]
  config: {allow: [bob], realm: x}
`,
			want: map[string][]string{"bob": {"qwen-72b"}, "alice": {}},
		},
		{
			name: "ingress and domain on one rule: route category, domain ignored",
			spec: `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen]
  domain: [nowhere.example.net]
  config: {allow: [alice]}
`,
			want: map[string][]string{"alice": {"qwen-72b"}},
		},
		{
			name: "two match keys on a rule without allow: the plugin's size count rejects it",
			spec: `
defaultConfig:` + consumersABC + `
matchRules:
- ingress: [qwen]
  domain: [api.example.com]
  config: {}
`,
			want:        map[string][]string{"alice": {}},
			wantWarning: "no consumers and no credentials",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := catalog(t, tt.spec, routes...)
			for consumer, want := range tt.want {
				if got := ids(c.Models(consumer, "api.example.com")); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: got %v, want %v", consumer, got, want)
				}
			}
			_, warnings := keyAuth(t, tt.spec)
			joined := strings.Join(warnings, "\n")
			if tt.wantWarning == "" && joined != "" {
				t.Errorf("unexpected warnings: %s", joined)
			}
			if tt.wantWarning != "" && !strings.Contains(joined, tt.wantWarning) {
				t.Errorf("warnings %q, want one containing %q", joined, tt.wantWarning)
			}
		})
	}
}

func TestPluginJSON(t *testing.T) {
	ka, _ := keyAuth(t, `
defaultConfig:
  global_auth: false
  consumers: [{name: a, credential: k}]
  keys: [k]
matchRules:
- ingress: [x]
  configDisable: true
  config: {allow: [a]}
- service: [svc.ns.svc.cluster.local]
  config: {allow: [a]}
`)
	if len(ka.Rules) != 1 || ka.Rules[0].category() != catService {
		t.Fatalf("rules: %+v", ka.Rules)
	}
	if _, ok := ka.Allowed("x", "h"); ok {
		t.Errorf("x: configDisable rule dropped, global_auth false: want open")
	}
}

func TestClassMatches(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := []struct {
		class      string
		annotation *string
		spec       *string
		want       bool
	}{
		{"higress", nil, str("higress"), true},
		{"higress", nil, nil, false},
		{"higress", str("higress"), str("nginx"), true}, // annotation wins
		{"higress", str("nginx"), str("higress"), false},
		{"nginx", nil, nil, true}, // nginx takes class-less Ingresses
		{"nginx", str(""), nil, true},
		{"nginx", nil, str("higress"), false},
		{"", str("anything"), nil, true},
	}
	for _, tt := range tests {
		ing := modelIngress("llm", "a", "m")
		ing.Spec.IngressClassName = tt.spec
		if tt.annotation != nil {
			ing.Annotations[IngressClassAnnotation] = *tt.annotation
		}
		if got := classMatches(ing, tt.class); got != tt.want {
			t.Errorf("class %q annotation %v spec %v: got %v", tt.class, tt.annotation, tt.spec, got)
		}
	}
}

func TestParseIngressExactAndPrefixIsSkipped(t *testing.T) {
	for range 20 { // map order is random; the result must not be
		ing := modelIngress("llm", "a", "m1", annotations("higress.io/prefix-match-header-x-higress-llm-model", "m"))
		if r, skipped := ParseIngress(ing, opts); r != nil || skipped == "" {
			t.Fatalf("got %+v %q", r, skipped)
		}
	}
}

func TestValidateKeyAuthPlugin(t *testing.T) {
	for v, ok := range map[string]bool{
		"":                          true,
		"higress-system/key-auth":   true,
		"key-auth":                  false,
		"/key-auth":                 false,
		"higress-system/":           false,
		"higress-system/key-auth/x": false,
	} {
		if err := ValidateKeyAuthPlugin(v); (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		}
	}
}
