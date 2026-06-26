// Copyright Contributors to the Open Cluster Management project

package security

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// Scanner evaluates resources against security checks defined in a SecurityConfig.
type Scanner struct {
	configLoader *ConfigLoader

	// regexCache avoids recompiling patterns on every scan.
	regexMu    sync.RWMutex
	regexCache map[string]*regexp.Regexp
}

// NewScanner creates a Scanner backed by the given ConfigLoader.
func NewScanner(loader *ConfigLoader) *Scanner {
	return &Scanner{
		configLoader: loader,
		regexCache:   make(map[string]*regexp.Regexp),
	}
}

// Scan evaluates all enabled checks against the resource and returns any findings.
func (s *Scanner) Scan(r *unstructured.Unstructured) []Finding {
	cfg := s.configLoader.GetConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	// Skip the scanner's own ConfigMap to avoid false positives from the regex patterns in the config.
	if r.GetKind() == "ConfigMap" && r.GetName() == defaultConfigMapName {
		return nil
	}

	kind := r.GetKind()
	apiGroup := r.GroupVersionKind().Group

	var findings []Finding
	for i := range cfg.Checks {
		check := &cfg.Checks[i]
		if !s.matchesResource(check, kind, apiGroup) {
			continue
		}
		if s.evaluateCheck(check, r) {
			findings = append(findings, Finding{
				Check:       check.Name,
				Severity:    check.Severity,
				Description: check.Description,
				Kind:        kind,
				Name:        r.GetName(),
				Namespace:   r.GetNamespace(),
			})
		}
	}
	return findings
}

// matchesResource checks if the check's match criteria apply to this resource.
func (s *Scanner) matchesResource(check *Check, kind, apiGroup string) bool {
	kindMatch := false
	for _, k := range check.Match.Kinds {
		if k == "*" || k == kind {
			kindMatch = true
			break
		}
	}
	if !kindMatch {
		return false
	}

	groupMatch := false
	for _, g := range check.Match.APIGroups {
		if g == "*" || g == apiGroup {
			groupMatch = true
			break
		}
	}
	return groupMatch
}

// evaluateCheck returns true if ALL rules in the check match (AND logic).
func (s *Scanner) evaluateCheck(check *Check, r *unstructured.Unstructured) bool {
	for i := range check.Rules {
		if !s.evaluateRule(&check.Rules[i], r) {
			return false
		}
	}
	return true
}

// evaluateRule returns true if ANY path in the rule matches (OR across paths).
func (s *Scanner) evaluateRule(rule *Rule, r *unstructured.Unstructured) bool {
	for _, path := range rule.Paths {
		values := resolvePath(r.Object, path)
		if s.evaluateOperator(rule.Operator, rule.Value, values) {
			return true
		}
	}
	return false
}

// resolvePath extracts values from an unstructured object given a dot-separated path.
// Supports [*] for array wildcards.
func resolvePath(obj interface{}, path string) []interface{} {
	// Strip leading dot.
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return []interface{}{obj}
	}

	parts := splitPath(path)
	return resolvePathParts(obj, parts)
}

// splitPath splits a dot-path like ".spec.containers[*].image" into segments.
func splitPath(path string) []string {
	var parts []string
	current := ""
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else if path[i] == '[' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
			// Find closing bracket.
			j := strings.IndexByte(path[i:], ']')
			if j < 0 {
				current = path[i:]
				break
			}
			parts = append(parts, path[i:i+j+1])
			i += j
		} else {
			current += string(path[i])
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// resolvePathParts recursively resolves path segments against a value.
func resolvePathParts(obj interface{}, parts []string) []interface{} {
	if len(parts) == 0 {
		return []interface{}{obj}
	}

	part := parts[0]
	rest := parts[1:]

	// Array wildcard — fan out over all elements.
	if part == "[*]" {
		arr, ok := obj.([]interface{})
		if !ok {
			return nil
		}
		var results []interface{}
		for _, elem := range arr {
			results = append(results, resolvePathParts(elem, rest)...)
		}
		return results
	}

	// Map key lookup.
	m, ok := obj.(map[string]interface{})
	if !ok {
		return nil
	}
	val, exists := m[part]
	if !exists {
		return nil
	}
	return resolvePathParts(val, rest)
}

// evaluateOperator applies the operator logic against resolved values.
func (s *Scanner) evaluateOperator(operator string, ruleValue interface{}, values []interface{}) bool {
	switch operator {
	case "equals":
		return s.opEquals(ruleValue, values)
	case "notEquals":
		return s.opNotEquals(ruleValue, values)
	case "contains":
		return s.opContains(ruleValue, values)
	case "notContains":
		return s.opNotContains(ruleValue, values)
	case "regex":
		return s.opRegex(ruleValue, values)
	case "valueMatchesRegex":
		return s.opValueMatchesRegex(ruleValue, values)
	case "keyMatchesRegex":
		return s.opKeyMatchesRegex(ruleValue, values)
	case "valueLengthGreaterThan":
		return s.opValueLengthGreaterThan(ruleValue, values)
	case "in":
		return s.opIn(ruleValue, values)
	case "exists":
		return len(values) > 0
	case "notExists":
		return len(values) == 0
	case "greaterThan":
		return s.opGreaterThan(ruleValue, values)
	default:
		klog.Warningf("Unknown security check operator: %s", operator)
		return false
	}
}

// opEquals returns true if any resolved value equals the rule value.
func (s *Scanner) opEquals(ruleValue interface{}, values []interface{}) bool {
	for _, v := range values {
		if fmt.Sprintf("%v", v) == fmt.Sprintf("%v", ruleValue) {
			return true
		}
	}
	return false
}

// opNotEquals returns true if no resolved value equals the rule value.
func (s *Scanner) opNotEquals(ruleValue interface{}, values []interface{}) bool {
	return !s.opEquals(ruleValue, values)
}

// opContains returns true if any resolved string value contains the substring.
func (s *Scanner) opContains(ruleValue interface{}, values []interface{}) bool {
	substr := fmt.Sprintf("%v", ruleValue)
	for _, v := range values {
		if str, ok := v.(string); ok && strings.Contains(str, substr) {
			return true
		}
	}
	return false
}

// opNotContains returns true if no resolved string value contains the substring.
func (s *Scanner) opNotContains(ruleValue interface{}, values []interface{}) bool {
	substr := fmt.Sprintf("%v", ruleValue)
	for _, v := range values {
		if str, ok := v.(string); ok && strings.Contains(str, substr) {
			return false
		}
	}
	return len(values) > 0 // only true if there were values to check
}

// opRegex returns true if any resolved string value matches the regex pattern.
func (s *Scanner) opRegex(ruleValue interface{}, values []interface{}) bool {
	re := s.getRegex(fmt.Sprintf("%v", ruleValue))
	if re == nil {
		return false
	}
	for _, v := range values {
		if str, ok := v.(string); ok && re.MatchString(str) {
			return true
		}
	}
	return false
}

// opValueMatchesRegex returns true if any value in any resolved map matches the regex.
func (s *Scanner) opValueMatchesRegex(ruleValue interface{}, values []interface{}) bool {
	re := s.getRegex(fmt.Sprintf("%v", ruleValue))
	if re == nil {
		return false
	}
	for _, v := range values {
		switch val := v.(type) {
		case map[string]interface{}:
			for _, mapVal := range val {
				if str, ok := mapVal.(string); ok && re.MatchString(str) {
					return true
				}
			}
		case string:
			if re.MatchString(val) {
				return true
			}
		}
	}
	return false
}

// opKeyMatchesRegex returns true if any key in any resolved map matches the regex.
func (s *Scanner) opKeyMatchesRegex(ruleValue interface{}, values []interface{}) bool {
	re := s.getRegex(fmt.Sprintf("%v", ruleValue))
	if re == nil {
		return false
	}
	for _, v := range values {
		if m, ok := v.(map[string]interface{}); ok {
			for key := range m {
				if re.MatchString(key) {
					return true
				}
			}
		}
	}
	return false
}

// opValueLengthGreaterThan returns true if any string value in any resolved map exceeds the threshold.
func (s *Scanner) opValueLengthGreaterThan(ruleValue interface{}, values []interface{}) bool {
	threshold := toFloat64(ruleValue)
	for _, v := range values {
		switch val := v.(type) {
		case map[string]interface{}:
			for _, mapVal := range val {
				if str, ok := mapVal.(string); ok && float64(len(str)) > threshold {
					return true
				}
			}
		case string:
			if float64(len(val)) > threshold {
				return true
			}
		}
	}
	return false
}

// opIn returns true if any resolved value is in the rule's list.
func (s *Scanner) opIn(ruleValue interface{}, values []interface{}) bool {
	list, ok := ruleValue.([]interface{})
	if !ok {
		return false
	}
	for _, v := range values {
		vStr := fmt.Sprintf("%v", v)
		for _, item := range list {
			if fmt.Sprintf("%v", item) == vStr {
				return true
			}
		}
	}
	return false
}

// opGreaterThan returns true if any resolved numeric value is greater than the rule value.
func (s *Scanner) opGreaterThan(ruleValue interface{}, values []interface{}) bool {
	threshold := toFloat64(ruleValue)
	for _, v := range values {
		if toFloat64(v) > threshold {
			return true
		}
	}
	return false
}

// getRegex returns a compiled regex, using the cache to avoid recompilation.
func (s *Scanner) getRegex(pattern string) *regexp.Regexp {
	s.regexMu.RLock()
	re, ok := s.regexCache[pattern]
	s.regexMu.RUnlock()
	if ok {
		return re
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		klog.Errorf("Invalid regex pattern in security check: %q: %v", pattern, err)
		return nil
	}

	s.regexMu.Lock()
	s.regexCache[pattern] = compiled
	s.regexMu.Unlock()
	return compiled
}

// toFloat64 converts a value to float64 for numeric comparisons.
func toFloat64(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case int32:
		return float64(val)
	default:
		return 0
	}
}
