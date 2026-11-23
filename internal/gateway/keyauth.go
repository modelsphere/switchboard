package gateway

import (
	"fmt"
	"maps"
	"strings"
)

// KeyAuth is the key-auth plugin config as the plugin itself parsed it.
// ParseKeyAuth reproduces two stages, each mirroring Higress source:
//
//  1. the controller turning the WasmPlugin CR into the plugin's JSON config
//     (pkg/ingress/config/ingress_config.go convertIstioWasmPlugin);
//  2. the plugin parsing that JSON (plugins/wasm-cpp/common/route_rule_matcher.h
//     parseAuthRuleConfig, key_auth/plugin.cc parsePluginConfig).
//
// Allowed then mirrors route_rule_matcher.h getMatchAuthConfig/checkAuthRule
// and key_auth checkAllowSet.
type KeyAuth struct {
	// Invalid: the plugin (or controller) rejects this config. Nothing is
	// listed for anyone.
	Invalid bool
	// GlobalAuth is the instance-level global_auth; nil when unset.
	GlobalAuth *bool
	// Global is the instance-level consumer config; nil when absent or when
	// the plugin failed to parse it.
	Global *ConsumerConfig
	Rules  []AuthRule
}

// ConsumerConfig is a key-auth config the plugin accepted: the consumer names
// it can authenticate (which become X-Mse-Consumer).
type ConsumerConfig struct {
	Consumers []string
}

// AuthRule is one entry of the plugin's _rules_.
type AuthRule struct {
	Routes        map[string]struct{}
	RoutePrefixes []string
	Domains       []domainMatch
	Services      []string
	// Allow is the rule's allow set. key-auth uses it whenever the rule has no
	// local config, so a rule without "allow" behaves as an empty set.
	Allow map[string]struct{}
	// Local is the rule's own consumer config; when set it replaces the global
	// config and Allow is ignored.
	Local    *ConsumerConfig
	Disabled bool
}

type category int

const (
	catRoute category = iota
	catRouteAndService
	catRoutePrefix
	catService
	catHost
)

// category follows parseAuthRuleConfig: route, then route prefix, then
// service, then host. Lower-priority match keys on the same rule are ignored.
func (r AuthRule) category() category {
	switch {
	case len(r.Routes) > 0 && len(r.Services) > 0:
		return catRouteAndService
	case len(r.Routes) > 0:
		return catRoute
	case len(r.RoutePrefixes) > 0:
		return catRoutePrefix
	case len(r.Services) > 0:
		return catService
	default:
		return catHost
	}
}

type domainMatch struct {
	kind  byte // 'e' exact, 's' suffix, 'p' prefix
	value string
}

func parseDomain(d string) domainMatch {
	switch {
	case strings.HasPrefix(d, "*"):
		return domainMatch{'s', strings.ToLower(d[1:])}
	case strings.HasSuffix(d, "*"):
		return domainMatch{'p', strings.ToLower(d[:len(d)-1])}
	default:
		return domainMatch{'e', strings.ToLower(d)}
	}
}

func (r AuthRule) hostMatches(host string) bool {
	if len(r.Domains) == 0 {
		return true
	}
	host = strings.ToLower(host)
	for _, d := range r.Domains {
		switch d.kind {
		case 's':
			if strings.HasSuffix(host, d.value) {
				return true
			}
		case 'p':
			if strings.HasPrefix(host, d.value) {
				return true
			}
		default:
			if host == d.value {
				return true
			}
		}
	}
	return false
}

// ParseKeyAuth reads the spec of a key-auth WasmPlugin (unstructured). The
// returned warnings describe config switchboard cannot evaluate exactly, or
// config the gateway rejects.
func ParseKeyAuth(spec map[string]any) (KeyAuth, []string) {
	raw, present, err := PluginJSON(spec)
	if err != nil {
		return KeyAuth{Invalid: true}, []string{err.Error()}
	}
	if !present {
		// The controller does not apply the plugin at all: no route is
		// authenticated by it.
		return KeyAuth{}, []string{"key-auth WasmPlugin has no config to apply (defaultConfigDisable and no matchRules)"}
	}
	return parsePluginJSON(raw)
}

// PluginJSON converts a WasmPlugin spec into the JSON config the plugin
// receives, as convertIstioWasmPlugin does. present is false when the
// controller applies no plugin.
func PluginJSON(spec map[string]any) (raw map[string]any, present bool, err error) {
	if pc, ok := spec["pluginConfig"].(map[string]any); ok {
		return pc, true, nil
	}
	var out map[string]any
	disableDefault, _ := spec["defaultConfigDisable"].(bool)
	if !disableDefault {
		if dc, ok := spec["defaultConfig"].(map[string]any); ok {
			out = copyMap(dc)
		}
	}
	hasValidRule := false
	if rules, _ := spec["matchRules"].([]any); len(rules) > 0 {
		if out == nil {
			out = map[string]any{}
		}
		var converted []any
		for i, item := range rules {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if b, _ := m["configDisable"].(bool); b {
				continue
			}
			rule := map[string]any{}
			if cfg, ok := m["config"].(map[string]any); ok {
				rule = copyMap(cfg)
			}
			valid := false
			for _, kv := range [][2]string{{"ingress", "_match_route_"}, {"service", "_match_service_"}, {"domain", "_match_domain_"}} {
				if list, _ := m[kv[0]].([]any); len(list) > 0 {
					rule[kv[1]] = list
					valid = true
				}
			}
			if !valid {
				return nil, false, fmt.Errorf("matchRules[%d] has no ingress/domain/service; the controller rejects the plugin", i)
			}
			converted = append(converted, rule)
		}
		if len(converted) > 0 {
			hasValidRule = true
			out["_rules_"] = converted
		}
	}
	if !hasValidRule && disableDefault {
		return nil, false, nil
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, true, nil
}

// parsePluginJSON mirrors parseAuthRuleConfig. An empty config is never
// parsed by the plugin (onConfigure skips size 0): no auth anywhere.
func parsePluginJSON(cfg map[string]any) (KeyAuth, []string) {
	var ka KeyAuth
	var warnings []string
	invalid := func(format string, a ...any) (KeyAuth, []string) {
		return KeyAuth{Invalid: true}, append(warnings, "key-auth config rejected by the plugin: "+fmt.Sprintf(format, a...))
	}
	if len(cfg) == 0 {
		return ka, nil
	}

	rulesRaw, hasRules := cfg["_rules_"]
	keyCount := len(cfg)
	if hasRules {
		keyCount--
	}
	if v, ok := cfg["global_auth"]; ok {
		g, isBool := v.(bool)
		if !isBool {
			return invalid("global_auth is not a bool")
		}
		ka.GlobalAuth = &g
	}
	var globalErr error
	if keyCount > 0 {
		var cc *ConsumerConfig
		if cc, globalErr = parseConsumerConfig(cfg); globalErr == nil {
			ka.Global = cc
		} else if hasRules {
			// Tolerated by the plugin: there is just no global config.
			warnings = append(warnings, "key-auth instance config ignored by the plugin: "+globalErr.Error())
		}
	}
	if !hasRules {
		if ka.Global == nil {
			return invalid("no rules and no valid instance config: %v", globalErr)
		}
		return ka, warnings
	}
	rules, ok := rulesRaw.([]any)
	if !ok {
		return invalid("_rules_ is not a list")
	}
	for i, item := range rules {
		m, ok := item.(map[string]any)
		if !ok {
			return invalid("_rules_[%d] is not an object", i)
		}
		var rule AuthRule
		local := len(m) - 1 // the plugin assumes exactly one _match_* key
		allowRaw, hasAllow := m["allow"]
		rule.Allow = map[string]struct{}{}
		if hasAllow {
			local--
			list, err := stringList(allowRaw, false)
			if err != nil {
				return invalid("_rules_[%d].allow: %v", i, err)
			}
			for _, s := range list {
				rule.Allow[s] = struct{}{}
			}
		}
		if v, ok := m["_disable_"]; ok {
			local--
			rule.Disabled, _ = v.(bool)
		}
		if local > 0 {
			cc, err := parseConsumerConfig(m)
			switch {
			case err == nil:
				rule.Local = cc
			case !hasAllow:
				return invalid("_rules_[%d]: %v", i, err)
			}
		}
		routes, err := matchList(m, "_match_route_")
		if err != nil {
			return invalid("_rules_[%d]: %v", i, err)
		}
		rule.Routes = setOf(routes)
		if rule.RoutePrefixes, err = matchList(m, "_match_route_prefix_"); err != nil {
			return invalid("_rules_[%d]: %v", i, err)
		}
		if rule.Services, err = matchList(m, "_match_service_"); err != nil {
			return invalid("_rules_[%d]: %v", i, err)
		}
		domains, err := matchList(m, "_match_domain_")
		if err != nil {
			return invalid("_rules_[%d]: %v", i, err)
		}
		for _, d := range domains {
			rule.Domains = append(rule.Domains, parseDomain(d))
		}
		if len(rule.Routes)+len(rule.RoutePrefixes)+len(rule.Services)+len(rule.Domains) == 0 {
			return invalid("_rules_[%d] has no match condition", i)
		}
		if c := rule.category(); c == catService || c == catRouteAndService {
			warnings = append(warnings, fmt.Sprintf("key-auth rule %d matches on service; switchboard treats it as never matching", i))
		}
		ka.Rules = append(ka.Rules, rule)
	}
	return ka, warnings
}

// parseConsumerConfig mirrors key_auth parsePluginConfig's accept/reject
// decision and returns the consumer names it would register.
func parseConsumerConfig(cfg map[string]any) (*ConsumerConfig, error) {
	_, hasConsumers := cfg["consumers"]
	credsRaw, hasCredentials := cfg["credentials"]
	switch {
	case hasConsumers && hasCredentials:
		return nil, fmt.Errorf("consumers and credentials cannot both be set")
	case !hasConsumers && !hasCredentials:
		return nil, fmt.Errorf("no consumers and no credentials")
	}

	checkInFlags := func(m map[string]any, where string) error {
		inQuery, inHeader := true, true
		for _, k := range []string{"in_query", "in_header"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			b, isBool := v.(bool)
			if !isBool {
				return fmt.Errorf("%s%s is not a bool", where, k)
			}
			if k == "in_query" {
				inQuery = b
			} else {
				inHeader = b
			}
		}
		if !inQuery && !inHeader {
			return fmt.Errorf("%sin_query and in_header are both false", where)
		}
		return nil
	}

	if hasCredentials {
		if _, err := stringList(credsRaw, true); err != nil {
			return nil, fmt.Errorf("credentials: %v", err)
		}
		keys, err := stringList(cfg["keys"], true)
		if err != nil {
			return nil, fmt.Errorf("keys: %v", err)
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("credentials need at least one key")
		}
		if v, ok := cfg["realm"]; ok {
			if _, isString := v.(string); !isString {
				return nil, fmt.Errorf("realm is not a string")
			}
		}
		if err := checkInFlags(cfg, ""); err != nil {
			return nil, err
		}
		// Anonymous credentials: no consumer name, so no X-Mse-Consumer.
		return &ConsumerConfig{}, nil
	}

	list, ok := cfg["consumers"].([]any)
	if !ok {
		return nil, fmt.Errorf("consumers is not a list")
	}
	cc := &ConsumerConfig{}
	seen := map[string]struct{}{}
	addCred := func(v any) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("credential is not a string")
		}
		if _, dup := seen[s]; dup {
			return fmt.Errorf("duplicate consumer credential")
		}
		seen[s] = struct{}{}
		return nil
	}
	needGlobalKeys := false
	for i, item := range list {
		c, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("consumers[%d] is not an object", i)
		}
		name, ok := c["name"].(string)
		if !ok {
			return nil, fmt.Errorf("consumers[%d].name missing or not a string", i)
		}
		cred, hasCred := c["credential"]
		creds, hasCreds := c["credentials"]
		switch {
		case hasCred && hasCreds:
			return nil, fmt.Errorf("consumers[%d] has both credential and credentials", i)
		case !hasCred && !hasCreds:
			return nil, fmt.Errorf("consumers[%d] has no credential", i)
		case hasCred:
			if err := addCred(cred); err != nil {
				return nil, fmt.Errorf("consumers[%d]: %v", i, err)
			}
		default:
			cl, ok := creds.([]any)
			if !ok {
				return nil, fmt.Errorf("consumers[%d].credentials is not a list", i)
			}
			for _, v := range cl {
				if err := addCred(v); err != nil {
					return nil, fmt.Errorf("consumers[%d]: %v", i, err)
				}
			}
		}
		if k, ok := c["keys"]; !ok {
			needGlobalKeys = true
		} else {
			if _, err := stringList(k, false); err != nil {
				return nil, fmt.Errorf("consumers[%d].keys: %v", i, err)
			}
			for _, f := range []string{"in_query", "in_header"} {
				if v, ok := c[f]; ok {
					if _, isBool := v.(bool); !isBool {
						return nil, fmt.Errorf("consumers[%d].%s is not a bool", i, f)
					}
				}
			}
		}
		cc.Consumers = append(cc.Consumers, name)
	}
	if needGlobalKeys {
		if _, err := stringList(cfg["keys"], true); err != nil {
			return nil, fmt.Errorf("keys: %v", err)
		}
		if err := checkInFlags(cfg, ""); err != nil {
			return nil, err
		}
	}
	return cc, nil
}

// Allowed returns the consumers key-auth lets through on the given route and
// request host. ok is false when key-auth does not authenticate requests there
// at all (route open, or config rejected): such a route is listed for nobody,
// because X-Mse-Consumer cannot identify who called it.
func (ka KeyAuth) Allowed(routeName, host string) (consumers map[string]struct{}, ok bool) {
	if ka.Invalid {
		return nil, false
	}
	cfg := ka.Global
	var allow map[string]struct{}
	hasAllow := false

	if len(ka.Rules) > 0 {
		matched := false
		for _, r := range ka.Rules {
			var hit bool
			switch r.category() {
			case catRoute:
				_, hit = r.Routes[routeName]
			case catRoutePrefix:
				for _, p := range r.RoutePrefixes {
					if strings.HasPrefix(routeName, p) {
						hit = true
						break
					}
				}
			case catHost:
				hit = r.hostMatches(host)
			default:
				hit = false // service-scoped rules: not evaluated
			}
			if !hit {
				continue
			}
			matched = true
			if r.Disabled {
				return nil, false
			}
			if r.Local != nil {
				cfg = r.Local
			} else {
				allow, hasAllow = r.Allow, true
			}
			break
		}
		if !matched && (ka.GlobalAuth == nil || !*ka.GlobalAuth) {
			return nil, false
		}
	}
	if cfg == nil {
		return nil, false
	}
	if !hasAllow && ka.GlobalAuth != nil && !*ka.GlobalAuth {
		return nil, false
	}
	out := map[string]struct{}{}
	for _, c := range cfg.Consumers {
		if !hasAllow {
			out[c] = struct{}{}
		} else if _, in := allow[c]; in {
			out[c] = struct{}{}
		}
	}
	return out, true
}

// stringList checks v is a list of strings. A missing value (nil) is an empty
// list when allowMissing, as JsonArrayIterate treats an absent field.
func stringList(v any, allowMissing bool) ([]string, error) {
	if v == nil {
		if allowMissing {
			return nil, nil
		}
		return nil, fmt.Errorf("not a list")
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("not a list")
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("has a non-string entry")
		}
		out = append(out, s)
	}
	return out, nil
}

func matchList(m map[string]any, key string) ([]string, error) {
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	list, err := stringList(v, false)
	if err != nil {
		return nil, fmt.Errorf("%s %v", key, err)
	}
	return list, nil
}

func setOf(list []string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, s := range list {
		m[s] = struct{}{}
	}
	return m
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	maps.Copy(out, m)
	return out
}
