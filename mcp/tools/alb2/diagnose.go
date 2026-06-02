package alb2

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ConflictWarning defines a single routing rule conflict/shadowing warning
type ConflictWarning struct {
	Severity         string `json:"severity"` // "Warning" or "Critical"
	Type             string `json:"type"`     // "WildcardShadowing", "PrefixShadowing", "DuplicateMatching"
	HighPriorityRule string `json:"highPriorityRule"`
	LowPriorityRule  string `json:"lowPriorityRule"`
	Message          string `json:"message"`
}

// Pattern represents an operator-value condition pair
type Pattern struct {
	Op    string `json:"op"`
	Value string `json:"value"`
}

// DSLXCondition represents a single DSLX match condition
type DSLXCondition struct {
	Type   string    `json:"type"`
	Key    string    `json:"key,omitempty"`
	Values []Pattern `json:"values,omitempty"`
}

type URLPattern = Pattern
type HostPattern = Pattern

// Dimension represents the routing dimension (matcher type and optional key)
type Dimension struct {
	Type string `json:"type"`
	Key  string `json:"key,omitempty"`
}

// ruleMatchInfo holds the normalized matching information extracted from a Rule
type ruleMatchInfo struct {
	Name                  string
	Priority              int64
	NormalizedConstraints map[Dimension][]Pattern
}

// DiagnoseALB2RuleConflictTool defines the tool schema
func DiagnoseALB2RuleConflictTool() mcp.Tool {
	return mcp.NewTool(
		"diagnose_alb2_rule_conflict",
		mcp.WithDescription("静态分析指定 Frontend 端口下是否存在路由规则优先级冲突、宽泛路径抢占精确路径（例如高优先级 rule-wildcard `/` 抢占低优先级 rule-specific `/manager`）等异常现象。/ Statically analyze routing rules under a Frontend for priority conflicts or shadowing issues."),
		mcp.WithTitleAnnotation("Diagnose ALB2 Rule Conflict"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行 ALB2 的集群名称 / Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("ALB2 所在的命名空间 / Namespace")),
		mcp.WithString("frontend_name", mcp.Description("进行诊断的 Frontend 监听器名称（与 rule 参数二选一），例如 center5-100-115-99-170-00080 / Target Frontend name (either frontend_name or rule is required)")),
		mcp.WithString("rule", mcp.Description("进行诊断的特定 Rule 规则名称（与 frontend_name 参数二选一）。若指定，则只返回与该规则相关的冲突警告。/ Specific Rule name to diagnose (either frontend_name or rule is required)")),
	)
}

// DiagnoseALB2RuleConflictHandler processes diagnose_alb2_rule_conflict request
func DiagnoseALB2RuleConflictHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	frontendName := request.GetString("frontend_name", "")
	ruleName := request.GetString("rule", "")

	if frontendName == "" && ruleName == "" {
		return nil, fmt.Errorf("either frontend_name or rule must be provided")
	}

	var ruleList []*unstructured.Unstructured

	// Query rules (try v1 first, fall back to v2beta1)
	err = kom.Cluster(meta.Cluster).WithContext(ctx).
		WithCache(time.Second*30).
		CRD("crd.alauda.io", "v1", "Rule").
		Namespace(meta.Namespace).
		List(&ruleList).Error

	if err != nil {
		err = kom.Cluster(meta.Cluster).WithContext(ctx).
			WithCache(time.Second*30).
			CRD("crd.alauda.io", "v2beta1", "Rule").
			Namespace(meta.Namespace).
			List(&ruleList).Error
		if err != nil {
			return nil, fmt.Errorf("failed to list Rule resources for diagnosis: %v", err)
		}
	}

	if frontendName == "" && ruleName != "" {
		for _, rule := range ruleList {
			if rule.GetName() == ruleName {
				frontendLabel, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/frontend")
				if frontendLabel != "" {
					frontendName = frontendLabel
					break
				}
			}
		}
		if frontendName == "" {
			return nil, fmt.Errorf("failed to find frontend for rule %s or rule does not exist in namespace %s", ruleName, meta.Namespace)
		}
	}

	// Filter rules belonging to the specified frontend and deduplicate by namespace/name
	var matchedRules []*unstructured.Unstructured
	seenRules := make(map[string]bool)
	for _, rule := range ruleList {
		frontendLabel, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/frontend")
		if frontendLabel == frontendName {
			key := rule.GetNamespace() + "/" + rule.GetName()
			if seenRules[key] {
				continue
			}
			seenRules[key] = true
			matchedRules = append(matchedRules, rule)
		}
	}

	warnings := analyzeRuleConflicts(matchedRules)

	if ruleName != "" {
		var filteredWarnings []ConflictWarning
		for _, w := range warnings {
			if w.HighPriorityRule == ruleName || w.LowPriorityRule == ruleName {
				filteredWarnings = append(filteredWarnings, w)
			}
		}
		warnings = filteredWarnings
	}

	type DiagnosisResult struct {
		FrontendName string            `json:"frontendName"`
		RulesCount   int               `json:"rulesCount"`
		Warnings     []ConflictWarning `json:"warnings"`
		Status       string            `json:"status"`
	}

	status := "Clean"
	if len(warnings) > 0 {
		status = "ConflictDetected"
	}

	result := DiagnosisResult{
		FrontendName: frontendName,
		RulesCount:   len(matchedRules),
		Warnings:     warnings,
		Status:       status,
	}

	return tools.TextResult(result, meta)
}

// parseDSLXValues parses the values nested structure in DSLX condition entries.
func parseDSLXValues(valuesRaw interface{}) []Pattern {
	valuesList, ok := valuesRaw.([]interface{})
	if !ok {
		return nil
	}
	var patterns []Pattern
	for _, valGroup := range valuesList {
		groupSlice, ok := valGroup.([]interface{})
		if !ok || len(groupSlice) < 1 {
			continue
		}
		op, _ := groupSlice[0].(string)
		op = strings.ToUpper(op)
		if op == "EXIST" {
			patterns = append(patterns, Pattern{
				Op:    op,
				Value: "",
			})
			continue
		}
		if len(groupSlice) < 2 {
			continue
		}
		if op == "IN" {
			for i := 1; i < len(groupSlice); i++ {
				val, _ := groupSlice[i].(string)
				patterns = append(patterns, Pattern{
					Op:    op,
					Value: val,
				})
			}
		} else if op == "RANGE" && len(groupSlice) >= 3 {
			val1, _ := groupSlice[1].(string)
			val2, _ := groupSlice[2].(string)
			patterns = append(patterns, Pattern{
				Op:    op,
				Value: val1 + "-" + val2,
			})
		} else {
			val, _ := groupSlice[1].(string)
			patterns = append(patterns, Pattern{
				Op:    op,
				Value: val,
			})
		}
	}
	return patterns
}

// extractDSLXConditions extracts normalized match conditions from a Rule resource
func extractDSLXConditions(rule *unstructured.Unstructured) []DSLXCondition {
	var conditions []DSLXCondition

	// 1. Try parsing spec.dslx first
	dslxSlice, dslxFound, _ := unstructured.NestedSlice(rule.Object, "spec", "dslx")
	if dslxFound && len(dslxSlice) > 0 {
		for _, entry := range dslxSlice {
			entryMap, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			entryType, _ := entryMap["type"].(string)
			entryType = strings.ToUpper(entryType)
			if entryType == "" {
				continue
			}

			key, _ := entryMap["key"].(string)

			var values []Pattern
			if valuesRaw, ok := entryMap["values"]; ok {
				values = parseDSLXValues(valuesRaw)
			}

			// Sort values to make semantic comparison order-independent
			sortPatterns(values)

			conditions = append(conditions, DSLXCondition{
				Type:   entryType,
				Key:    key,
				Values: values,
			})
		}
	} else {
		// 2. Fall back to legacy fields (spec.url, spec.domain, spec.dsl)
		url, _, _ := unstructured.NestedString(rule.Object, "spec", "url")
		if url != "" {
			conditions = append(conditions, DSLXCondition{
				Type:   "URL",
				Values: []Pattern{{Op: "STARTS_WITH", Value: url}},
			})
		}

		domain, _, _ := unstructured.NestedString(rule.Object, "spec", "domain")
		if domain != "" {
			op := "EQ"
			if strings.Contains(domain, "*") || strings.HasPrefix(domain, ".") {
				op = "ENDS_WITH"
			}
			conditions = append(conditions, DSLXCondition{
				Type:   "HOST",
				Values: []Pattern{{Op: op, Value: domain}},
			})
		}

		dsl, _, _ := unstructured.NestedString(rule.Object, "spec", "dsl")
		if dsl != "" {
			conditions = append(conditions, DSLXCondition{
				Type:   "DSL",
				Values: []Pattern{{Op: "DSL", Value: dsl}},
			})
		}
	}

	// Sort conditions by Type and Key to make semantic comparison order-independent
	sortConditions(conditions)

	return conditions
}

func sortPatterns(patterns []Pattern) {
	sort.Slice(patterns, func(i, j int) bool {
		if patterns[i].Op != patterns[j].Op {
			return patterns[i].Op < patterns[j].Op
		}
		return patterns[i].Value < patterns[j].Value
	})
}

func sortConditions(conds []DSLXCondition) {
	sort.Slice(conds, func(i, j int) bool {
		if conds[i].Type != conds[j].Type {
			return conds[i].Type < conds[j].Type
		}
		return conds[i].Key < conds[j].Key
	})
}

// extractRuleMatchInfo extracts normalized matching information from a Rule resource
func extractRuleMatchInfo(rule *unstructured.Unstructured) ruleMatchInfo {
	return ruleMatchInfo{
		Name:                  rule.GetName(),
		Priority:              getRulePriority(rule),
		NormalizedConstraints: getNormalizedRuleConstraints(rule),
	}
}

// getNormalizedRuleConstraints returns a normalized map of constraints by dimension.
// It automatically handles wildcard conditions (which are ignored, meaning no constraint on that dimension).
func getNormalizedRuleConstraints(rule *unstructured.Unstructured) map[Dimension][]Pattern {
	conditions := extractDSLXConditions(rule)
	constraints := make(map[Dimension][]Pattern)

	for _, c := range conditions {
		dim := Dimension{Type: c.Type, Key: c.Key}
		if len(c.Values) == 0 {
			continue
		}

		var cleanPatterns []Pattern
		hasWildcard := false
		for _, p := range c.Values {
			if isWildcard(dim, p) {
				hasWildcard = true
				break
			}
			cleanPatterns = append(cleanPatterns, p)
		}

		if hasWildcard {
			// A wildcard pattern matches any value, which makes the whole dimension unconstrained (OR logic)
			continue
		}

		if len(cleanPatterns) > 0 {
			// Ensure order-independent comparison later
			sortPatterns(cleanPatterns)
			constraints[dim] = cleanPatterns
		}
	}

	return constraints
}

func isWildcard(dim Dimension, p Pattern) bool {
	if dim.Type == "HOST" {
		return p.Value == "" || p.Value == "*"
	}
	if dim.Type == "URL" {
		if p.Op == "STARTS_WITH" || p.Op == "REGEX" {
			return isWildcardURLPath(p.Value)
		}
		return false
	}
	return false
}

func isWildcardURLPath(val string) bool {
	return val == "" || val == "/" || val == "/*"
}

func hostPatternCovers(a, b Pattern) bool {
	dA, sA := normalizeHostPattern(a)
	dB, sB := normalizeHostPattern(b)

	// If A matches all hosts, it covers B
	if dA == "" || dA == "*" {
		return true
	}
	// If A restricts host but B matches all hosts, A cannot cover B
	if dB == "" || dB == "*" {
		return false
	}

	if sA {
		if sB {
			// B matches subdomains of dB, which are also subdomains of dA.
			return dB == dA || strings.HasSuffix(dB, "."+dA)
		} else {
			// B is exact match.
			// A is suffix. Check if dB matches subdomain or the domain itself.
			requiresDot := strings.HasPrefix(a.Value, "*.") || strings.HasPrefix(a.Value, ".")
			if requiresDot {
				return dB == dA || strings.HasSuffix(dB, "."+dA)
			} else {
				return dB == dA || strings.HasSuffix(dB, "."+dA) || strings.HasSuffix(dB, dA)
			}
		}
	}

	// If A is exact match, it only covers B if B is exact match and they are equal.
	return !sA && !sB && dA == dB
}

func patternCoversPattern(a, b Pattern, dim Dimension) bool {
	// EXIST covers anything on the same key/dimension
	if a.Op == "EXIST" {
		return true
	}

	// Identical patterns always cover each other
	if a.Op == b.Op && a.Value == b.Value {
		return true
	}

	// Normalize HOST dimension checks
	if dim.Type == "HOST" {
		return hostPatternCovers(a, b)
	}

	// Normalize IP range checks for SRC_IP or when both can be parsed as IP ranges
	if dim.Type == "SRC_IP" || a.Op == "RANGE" || b.Op == "RANGE" {
		startA, endA, errA := parseIPRange(a.Value)
		startB, endB, errB := parseIPRange(b.Value)
		if errA == nil && errB == nil {
			sh16 := startA.To16()
			eh16 := endA.To16()
			sl16 := startB.To16()
			el16 := endB.To16()
			if sh16 != nil && eh16 != nil && sl16 != nil && el16 != nil {
				return bytes.Compare(sl16, sh16) >= 0 && bytes.Compare(el16, eh16) <= 0
			}
		}
	}

	// For normal string matchers, treat IN and EQ as equivalent
	opA := a.Op
	if opA == "IN" {
		opA = "EQ"
	}
	opB := b.Op
	if opB == "IN" {
		opB = "EQ"
	}

	// Exact match
	if opA == "EQ" && opB == "EQ" {
		return a.Value == b.Value
	}

	// Prefix match
	if opA == "STARTS_WITH" {
		if opB == "STARTS_WITH" || opB == "EQ" {
			return strings.HasPrefix(b.Value, a.Value)
		}
	}

	// Suffix match
	if opA == "ENDS_WITH" {
		if opB == "ENDS_WITH" || opB == "EQ" {
			return strings.HasSuffix(b.Value, a.Value)
		}
	}

	return false
}

func ruleA_Shadows_ruleB(constraintsA, constraintsB map[Dimension][]Pattern) (shadowType string) {
	// A covers B if:
	// For every dimension D in A:
	//   1. D must be present in B.
	//   2. For every pattern b in B[D], there must be some pattern a in A[D] that covers b.

	// If A has no constraints at all, it covers B (since A matches everything).
	if len(constraintsA) == 0 {
		if len(constraintsB) > 0 {
			return "WildcardShadowing"
		}
		return ""
	}

	hasSpecificShadow := false
	hasWildcardShadow := false

	// Track if B restricts any dimension that A does not.
	// Since A covers B (which we verify below), the omission of this dimension in A
	// acts as an implicit wildcard that shadows B's specific dimension.
	for dimB := range constraintsB {
		if _, exists := constraintsA[dimB]; !exists {
			hasWildcardShadow = true
			break
		}
	}

	for dimA, patternsA := range constraintsA {
		patternsB, exists := constraintsB[dimA]
		if !exists {
			// B does not constrain this dimension. Since A restricts it, A cannot cover B.
			return ""
		}

		// Check if patternsA covers patternsB
		for _, b := range patternsB {
			covered := false
			for _, a := range patternsA {
				if patternCoversPattern(a, b, dimA) {
					covered = true
					// Check the type of shadowing
					if a.Op == "STARTS_WITH" && b.Op == "STARTS_WITH" && a.Value != b.Value {
						hasSpecificShadow = true
					} else if dimA.Type == "URL" && isWildcardURLPath(a.Value) && !isWildcardURLPath(b.Value) {
						hasWildcardShadow = true
					} else if a.Op == "EXIST" && b.Op != "EXIST" {
						hasWildcardShadow = true // Existential covers specific value (e.g. cookie exist shadows cookie=val)
					} else if a.Op == "ENDS_WITH" && b.Op == "ENDS_WITH" && a.Value != b.Value {
						hasSpecificShadow = true
					} else if a.Op == "RANGE" && b.Op != "RANGE" {
						hasWildcardShadow = true // IP range covers specific IP
					} else if a.Op == "IN" && b.Op != "IN" {
						hasWildcardShadow = true // Set containing values covers specific value
					}
					break
				}
			}
			if !covered {
				return ""
			}
		}
	}

	// If we got here, A covers B.
	if hasWildcardShadow {
		return "WildcardShadowing"
	}
	if hasSpecificShadow {
		return "PrefixShadowing"
	}
	return "PrefixShadowing" // Default fallback for shadowing warning
}

func constraintsEqual(cA, cB map[Dimension][]Pattern) bool {
	if len(cA) != len(cB) {
		return false
	}
	for dimA, patternsA := range cA {
		patternsB, exists := cB[dimA]
		if !exists {
			return false
		}
		if len(patternsA) != len(patternsB) {
			return false
		}
		for i := range patternsA {
			if patternsA[i].Op != patternsB[i].Op || patternsA[i].Value != patternsB[i].Value {
				return false
			}
		}
	}
	return true
}

func parseIPRange(s string) (net.IP, net.IP, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		if len(parts) == 2 {
			start := net.ParseIP(strings.TrimSpace(parts[0]))
			end := net.ParseIP(strings.TrimSpace(parts[1]))
			if start != nil && end != nil {
				return start, end, nil
			}
		}
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip, ip, nil
	}
	if _, ipnet, err := net.ParseCIDR(s); err == nil {
		start := ipnet.IP
		end := make(net.IP, len(start))
		copy(end, start)
		for i := range end {
			end[i] |= ^ipnet.Mask[i]
		}
		return start, end, nil
	}
	return nil, nil, fmt.Errorf("invalid IP range: %s", s)
}

func normalizeHostPattern(hp Pattern) (domain string, isSuffix bool) {
	val := hp.Value
	if hp.Op == "ENDS_WITH" || strings.HasPrefix(val, "*.") || strings.HasPrefix(val, ".") {
		isSuffix = true
		val = strings.TrimPrefix(val, "*")
		val = strings.TrimPrefix(val, ".")
		return val, true
	}
	return val, false
}

func hostsEquivalent(h1, h2 Pattern) bool {
	d1, s1 := normalizeHostPattern(h1)
	d2, s2 := normalizeHostPattern(h2)

	if (d1 == "" || d1 == "*") && (d2 == "" || d2 == "*") {
		return true
	}
	return d1 == d2 && s1 == s2
}

func buildShadowingMessage(infoHigh, infoLow ruleMatchInfo, shadowType string, samePriority bool) string {
	urlDim := Dimension{Type: "URL"}
	hostDim := Dimension{Type: "HOST"}

	urlHigh := "/"
	if urls := infoHigh.NormalizedConstraints[urlDim]; len(urls) > 0 {
		urlHigh = urls[0].Value
	}
	urlLow := "/"
	if urls := infoLow.NormalizedConstraints[urlDim]; len(urls) > 0 {
		urlLow = urls[0].Value
	}

	displayHostHigh := "*"
	if hosts := infoHigh.NormalizedConstraints[hostDim]; len(hosts) > 0 && hosts[0].Value != "" {
		displayHostHigh = hosts[0].Value
	}

	displayHostLow := "*"
	if hosts := infoLow.NormalizedConstraints[hostDim]; len(hosts) > 0 && hosts[0].Value != "" {
		displayHostLow = hosts[0].Value
	}

	if samePriority {
		if shadowType == "WildcardShadowing" {
			return fmt.Sprintf("⚠️ 警告：规则 [%s] 与规则 [%s] 优先级相同 (优先级: %d)。较为宽泛的规则 [%s] (域名: `%s`, 路径: `%s`) 与规则 [%s] (域名: `%s`, 路径: `%s`) 存在匹配范围重叠，由于评估顺序不确定，可能导致请求被意外拦截。",
				infoHigh.Name, infoLow.Name, infoHigh.Priority, infoHigh.Name, displayHostHigh, urlHigh, infoLow.Name, displayHostLow, urlLow)
		} else {
			return fmt.Sprintf("⚠️ 警告：规则 [%s] 与规则 [%s] 优先级相同 (优先级: %d)。前缀匹配规则 [%s] (域名: `%s`, 路径: `%s`) 与规则 [%s] (域名: `%s`, 路径: `%s`) 存在匹配范围重叠，由于评估顺序不确定，可能导致请求被意外拦截。",
				infoHigh.Name, infoLow.Name, infoHigh.Priority, infoHigh.Name, displayHostHigh, urlHigh, infoLow.Name, displayHostLow, urlLow)
		}
	} else {
		if shadowType == "WildcardShadowing" {
			return fmt.Sprintf("⚠️ 警告：高优先级规则 [%s] (优先级: %d, 域名: `%s`, 路径: `%s`) 匹配了全部流量，这会导致低优先级规则 [%s] (优先级: %d, 域名: `%s`, 路径: `%s`) 被强行拦截屏蔽。",
				infoHigh.Name, infoHigh.Priority, displayHostHigh, urlHigh, infoLow.Name, infoLow.Priority, displayHostLow, urlLow)
		} else {
			return fmt.Sprintf("⚠️ 警告：高优先级规则 [%s] (优先级: %d, 域名: `%s`, 路径: `%s`) evaluation 优于具体规则 [%s] (优先级: %d, 域名: `%s`, 路径: `%s`)，将劫持其匹配范围内的所有请求。",
				infoHigh.Name, infoHigh.Priority, displayHostHigh, urlHigh, infoLow.Name, infoLow.Priority, displayHostLow, urlLow)
		}
	}
}

// sortMatchedRules sorts rules by priority (ascending), then alphabetically by namespace/name as tie-breaker
func sortMatchedRules(rules []*unstructured.Unstructured) {
	sort.Slice(rules, func(i, j int) bool {
		pI := getRulePriority(rules[i])
		pJ := getRulePriority(rules[j])
		if pI != pJ {
			return pI < pJ
		}
		keyI := rules[i].GetNamespace() + "/" + rules[i].GetName()
		keyJ := rules[j].GetNamespace() + "/" + rules[j].GetName()
		return keyI < keyJ
	})
}

// hasEquivalentHost checks if two rules belong to the same host routing bucket (share equivalent host patterns)
func hasEquivalentHost(cA, cB map[Dimension][]Pattern) bool {
	hostDim := Dimension{Type: "HOST"}
	hostsA := cA[hostDim]
	hostsB := cB[hostDim]

	if len(hostsA) == 0 && len(hostsB) == 0 {
		return true // Both have wildcard host
	}
	for _, a := range hostsA {
		for _, b := range hostsB {
			if hostsEquivalent(a, b) {
				return true
			}
		}
	}
	return false
}

// checkPairConflict checks if there's any conflict or shadowing warning between a pair of rules.
func checkPairConflict(infoI, infoJ ruleMatchInfo) []ConflictWarning {
	var warnings []ConflictWarning

	// 1. Duplicate matching: same matching semantics
	if constraintsEqual(infoI.NormalizedConstraints, infoJ.NormalizedConstraints) {
		urlDim := Dimension{Type: "URL"}
		urlDisplay := "/"
		if urls := infoI.NormalizedConstraints[urlDim]; len(urls) > 0 {
			urlDisplay = urls[0].Value
		}
		var msg string
		if infoI.Priority < infoJ.Priority {
			msg = fmt.Sprintf("⚠️ 关键冲突：Rule [%s] (优先级: %d) 与 Rule [%s] (优先级: %d) 拥有完全相同的匹配条件 (路径: %s)。低优先级的规则将永远无法被访问。",
				infoI.Name, infoI.Priority, infoJ.Name, infoJ.Priority, urlDisplay)
		} else {
			msg = fmt.Sprintf("⚠️ 关键冲突：Rule [%s] (优先级: %d) 与 Rule [%s] (优先级: %d) 拥有完全相同的匹配条件 (路径: %s)。两规则优先级相同，实际命中行为不确定，属于配置冗余。",
				infoI.Name, infoI.Priority, infoJ.Name, infoJ.Priority, urlDisplay)
		}
		warnings = append(warnings, ConflictWarning{
			Severity:         "Critical",
			Type:             "DuplicateMatching",
			HighPriorityRule: infoI.Name,
			LowPriorityRule:  infoJ.Name,
			Message:          msg,
		})
		return warnings
	}

	// 2. Shadowing check
	if infoI.Priority < infoJ.Priority {
		// High priority infoI shadows low priority infoJ
		if shadowType := ruleA_Shadows_ruleB(infoI.NormalizedConstraints, infoJ.NormalizedConstraints); shadowType != "" {
			warnings = append(warnings, ConflictWarning{
				Severity:         "Warning",
				Type:             shadowType,
				HighPriorityRule: infoI.Name,
				LowPriorityRule:  infoJ.Name,
				Message:          buildShadowingMessage(infoI, infoJ, shadowType, false),
			})
		}
	} else if infoI.Priority == infoJ.Priority {
		// Same priority: they can only shadow if they belong to the same host routing bucket (i.e. identical host patterns)
		if hasEquivalentHost(infoI.NormalizedConstraints, infoJ.NormalizedConstraints) {
			if shadowTypeIJ := ruleA_Shadows_ruleB(infoI.NormalizedConstraints, infoJ.NormalizedConstraints); shadowTypeIJ != "" {
				warnings = append(warnings, ConflictWarning{
					Severity:         "Warning",
					Type:             shadowTypeIJ,
					HighPriorityRule: infoI.Name,
					LowPriorityRule:  infoJ.Name,
					Message:          buildShadowingMessage(infoI, infoJ, shadowTypeIJ, true),
				})
			}
			if shadowTypeJI := ruleA_Shadows_ruleB(infoJ.NormalizedConstraints, infoI.NormalizedConstraints); shadowTypeJI != "" {
				warnings = append(warnings, ConflictWarning{
					Severity:         "Warning",
					Type:             shadowTypeJI,
					HighPriorityRule: infoJ.Name,
					LowPriorityRule:  infoI.Name,
					Message:          buildShadowingMessage(infoJ, infoI, shadowTypeJI, true),
				})
			}
		}
	}

	return warnings
}

// analyzeRuleConflicts statically analyzes rules for priority conflicts, duplicate matching, and shadowing issues.
func analyzeRuleConflicts(matchedRules []*unstructured.Unstructured) []ConflictWarning {
	var warnings []ConflictWarning

	// Sort rules by priority (ascending), then alphabetically by namespace/name as tie-breaker
	sortMatchedRules(matchedRules)

	// Pre-extract match info for all rules
	infos := make([]ruleMatchInfo, len(matchedRules))
	for i, rule := range matchedRules {
		infos[i] = extractRuleMatchInfo(rule)
	}

	// Compare each rule pair (i, j)
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			warnings = append(warnings, checkPairConflict(infos[i], infos[j])...)
		}
	}

	return warnings
}
