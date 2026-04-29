package config

import (
	"regexp"

	"github.com/mariotoffia/gobridge/ports"
)

var validResolverTypes = map[string]bool{
	"rules":      true,
	"header_map": true,
	"all":        true,
	"static":     true,
}

var validConditionOperators = map[string]bool{
	"eq": true, "ne": true, "prefix": true, "contains": true,
	"regex": true, "gt": true, "lt": true, "gte": true, "lte": true,
	"exists": true, "in": true,
}

const maxRegexPatternLen = 4096

func validateResolver(ve *ValidationError, prefix string, r ports.RouteDef) {
	res := r.Resolver
	if !validResolverTypes[res.Type] {
		ve.Addf("%s: resolver.type %q is invalid; must be one of: rules, header_map, all, static", prefix, res.Type)
		return
	}

	bindingSet := make(map[string]bool, len(r.Bindings))
	for _, bid := range r.Bindings {
		bindingSet[bid] = true
	}

	if res.DefaultBinding != "" && !bindingSet[res.DefaultBinding] {
		ve.Addf("%s: resolver.default_binding %q not found in route bindings", prefix, res.DefaultBinding)
	}

	switch res.Type {
	case "header_map":
		validateHeaderMapResolver(ve, prefix, res, bindingSet)
	case "rules":
		validateRulesResolver(ve, prefix, res, bindingSet)
	}
}

func validateHeaderMapResolver(ve *ValidationError, prefix string, res *ports.ResolverDef, bindingSet map[string]bool) {
	if res.HeaderKey == "" {
		ve.Addf("%s: resolver.header_key is required for header_map type", prefix)
	}
	if len(res.HeaderMap) == 0 {
		ve.Addf("%s: resolver.header_map must have at least one entry", prefix)
	}
	for val, bid := range res.HeaderMap {
		if !bindingSet[bid] {
			ve.Addf("%s: resolver.header_map[%q] references unknown binding %q", prefix, val, bid)
		}
	}
}

func validateRulesResolver(ve *ValidationError, prefix string, res *ports.ResolverDef, bindingSet map[string]bool) {
	if len(res.Rules) == 0 && res.DefaultBinding == "" {
		ve.Addf("%s: resolver type \"rules\" requires at least one rule or a default_binding", prefix)
	}
	for i, rule := range res.Rules {
		rp := prefix + ".resolver.rules[" + itoa(i) + "]"
		if rule.BindingID == "" {
			ve.Addf("%s: binding_id is required", rp)
		} else if !bindingSet[rule.BindingID] {
			ve.Addf("%s: binding_id %q not found in route bindings", rp, rule.BindingID)
		}
		for j, cond := range rule.Match {
			cp := rp + ".match[" + itoa(j) + "]"
			if cond.Field == "" {
				ve.Addf("%s: field is required", cp)
			}
			if cond.Operator == "" {
				ve.Addf("%s: operator is required", cp)
			} else if !validConditionOperators[cond.Operator] {
				ve.Addf("%s: operator %q is invalid", cp, cond.Operator)
			}
			if cond.Operator == "regex" {
				if pattern, ok := cond.Value.(string); ok {
					if len(pattern) > maxRegexPatternLen {
						ve.Addf("%s: regex pattern exceeds maximum length of %d characters", cp, maxRegexPatternLen)
					} else if _, err := regexp.Compile(pattern); err != nil {
						ve.Addf("%s: invalid regex pattern: %v", cp, err)
					}
				} else {
					ve.Addf("%s: regex operator requires a string value", cp)
				}
			}
		}
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}
	buf := make([]byte, 0, 4)
	for i > 0 {
		buf = append(buf, digits[i%10])
		i /= 10
	}
	for l, r := 0, len(buf)-1; l < r; l, r = l+1, r-1 {
		buf[l], buf[r] = buf[r], buf[l]
	}
	return string(buf)
}
