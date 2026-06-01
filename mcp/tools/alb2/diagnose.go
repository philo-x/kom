package alb2

import (
	"bytes"
	"context"
	"encoding/json"
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

// ruleMatchInfo holds the normalized matching information extracted from a Rule
type ruleMatchInfo struct {
	Name               string
	Priority           int64
	Conditions         []DSLXCondition
	URLs               []URLPattern
	Hosts              []HostPattern
	HasExtraConditions bool
	DslxJSON           string
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
	conditions := extractDSLXConditions(rule)
	var urls []URLPattern
	var hosts []HostPattern
	var hasExtra bool
	for _, c := range conditions {
		if c.Type == "URL" {
			urls = c.Values
		} else if c.Type == "HOST" {
			hosts = c.Values
		} else {
			hasExtra = true
		}
	}
	var dslxJSON string
	dslxSlice, dslxFound, _ := unstructured.NestedSlice(rule.Object, "spec", "dslx")
	if dslxFound && len(dslxSlice) > 0 {
		if bytes, err := json.Marshal(dslxSlice); err == nil {
			dslxJSON = string(bytes)
		}
	}

	return ruleMatchInfo{
		Name:               rule.GetName(),
		Priority:           getRulePriority(rule),
		Conditions:         conditions,
		URLs:               urls,
		Hosts:              hosts,
		HasExtraConditions: hasExtra,
		DslxJSON:           dslxJSON,
	}
}

// dslxConditionsEqual checks whether two lists of DSLXCondition are semantically identical
func dslxConditionsEqual(a, b []DSLXCondition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Key != b[i].Key {
			return false
		}
		if len(a[i].Values) != len(b[i].Values) {
			return false
		}
		for j := range a[i].Values {
			if a[i].Values[j].Op != b[i].Values[j].Op || a[i].Values[j].Value != b[i].Values[j].Value {
				return false
			}
		}
	}
	return true
}

func getRuleURLs(conds []DSLXCondition) []Pattern {
	for _, c := range conds {
		if c.Type == "URL" {
			return c.Values
		}
	}
	return []Pattern{{Op: "STARTS_WITH", Value: "/"}}
}

func getRuleHosts(conds []DSLXCondition) []Pattern {
	for _, c := range conds {
		if c.Type == "HOST" {
			return c.Values
		}
	}
	return []Pattern{{Op: "EQ", Value: "*"}}
}

func getRuleExtraConditions(conds []DSLXCondition) []DSLXCondition {
	var extra []DSLXCondition
	for _, c := range conds {
		if c.Type != "URL" && c.Type != "HOST" {
			extra = append(extra, c)
		}
	}
	return extra
}

func extraConditionsCover(extraI, extraJ []DSLXCondition) bool {
	if len(extraI) == 0 {
		return true
	}
	for _, cI := range extraI {
		found := false
		for _, cJ := range extraJ {
			if cI.Type == cJ.Type && cI.Key == cJ.Key {
				if patternsCover(cI.Values, cJ.Values) {
					found = true
					break
				}
			}
		}
		if !found {
			return false
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

func ipRangeCovers(rangeStr, valStr string) bool {
	startHigh, endHigh, err := parseIPRange(rangeStr)
	if err != nil {
		return false
	}
	startLow, endLow, err := parseIPRange(valStr)
	if err != nil {
		return false
	}
	sh16 := startHigh.To16()
	eh16 := endHigh.To16()
	sl16 := startLow.To16()
	el16 := endLow.To16()
	if sh16 == nil || eh16 == nil || sl16 == nil || el16 == nil {
		return false
	}
	return bytes.Compare(sl16, sh16) >= 0 && bytes.Compare(el16, eh16) <= 0
}

func patternCoversPattern(pi, pj Pattern) bool {
	if pi.Op == "EXIST" {
		return true
	}
	if pi.Op == pj.Op && pi.Value == pj.Value {
		return true
	}
	if pi.Op == "IN" && pj.Op == "EQ" && pi.Value == pj.Value {
		return true
	}
	if (pi.Op == "EQ" || pi.Op == "RANGE") && (pj.Op == "EQ" || pj.Op == "RANGE") {
		startHigh, endHigh, errHigh := parseIPRange(pi.Value)
		startLow, endLow, errLow := parseIPRange(pj.Value)
		if errHigh == nil && errLow == nil {
			sh16 := startHigh.To16()
			eh16 := endHigh.To16()
			sl16 := startLow.To16()
			el16 := endLow.To16()
			if sh16 != nil && eh16 != nil && sl16 != nil && el16 != nil {
				return bytes.Compare(sl16, sh16) >= 0 && bytes.Compare(el16, eh16) <= 0
			}
		}
	}
	return false
}

func patternsCover(pI, pJ []Pattern) bool {
	for _, pj := range pJ {
		matched := false
		for _, pi := range pI {
			if patternCoversPattern(pi, pj) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
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

func hostPatternsOverlap(h1, h2 Pattern) bool {
	d1, s1 := normalizeHostPattern(h1)
	d2, s2 := normalizeHostPattern(h2)

	if d1 == "" || d1 == "*" || d2 == "" || d2 == "*" {
		return true
	}

	if s1 && s2 {
		return d1 == d2 || strings.HasSuffix(d1, "."+d2) || strings.HasSuffix(d2, "."+d1)
	}
	if !s1 && !s2 {
		return d1 == d2
	}
	if s1 {
		return d2 == d1 || strings.HasSuffix(d2, "."+d1)
	} else {
		return d1 == d2 || strings.HasSuffix(d1, "."+d2)
	}
}

func hostPatternShadows(h1, h2 Pattern) bool {
	d1, s1 := normalizeHostPattern(h1)
	d2, s2 := normalizeHostPattern(h2)

	if d1 == "" || d1 == "*" {
		return d2 != "" && d2 != "*"
	}

	if s1 {
		if s2 {
			return d1 != d2 && strings.HasSuffix(d2, "."+d1)
		} else {
			requiresDot := strings.HasPrefix(h1.Value, "*.") || strings.HasPrefix(h1.Value, ".")
			if requiresDot {
				return strings.HasSuffix(d2, "."+d1)
			} else {
				return d2 == d1 || strings.HasSuffix(d2, "."+d1) || strings.HasSuffix(d2, d1)
			}
		}
	}
	return false
}

func hostsEquivalent(h1, h2 Pattern) bool {
	d1, s1 := normalizeHostPattern(h1)
	d2, s2 := normalizeHostPattern(h2)

	if (d1 == "" || d1 == "*") && (d2 == "" || d2 == "*") {
		return true
	}
	return d1 == d2 && s1 == s2
}

func domainsOverlap(hostsA, hostsB []Pattern) bool {
	if len(hostsA) == 0 || len(hostsB) == 0 {
		return true
	}

	for _, a := range hostsA {
		for _, b := range hostsB {
			if hostPatternsOverlap(a, b) {
				return true
			}
		}
	}
	return false
}

func getEffectiveURLFromConds(conds []DSLXCondition) string {
	for _, c := range conds {
		if c.Type == "URL" && len(c.Values) > 0 {
			return c.Values[0].Value
		}
	}
	return ""
}

func isWildcardURL(url Pattern) bool {
	return url.Value == "" || url.Value == "/" || (url.Op == "STARTS_WITH" && url.Value == "/")
}

func urlShadowsURL(urlA, urlB Pattern) (shadowType string) {
	if isWildcardURL(urlA) && !isWildcardURL(urlB) {
		return "WildcardShadowing"
	}

	if urlA.Op == "REGEX" || urlB.Op == "REGEX" {
		return ""
	}

	if urlA.Op == "STARTS_WITH" && urlA.Value != "" && urlB.Value != "" {
		valA := urlA.Value
		valB := urlB.Value
		if strings.HasPrefix(valB, valA) && valA != valB {
			return "PrefixShadowing"
		}
	}

	return ""
}

func urlPatternsCover(urlsI, urlsJ []Pattern) (shadowType string) {
	isIdentical := true
	for _, uj := range urlsJ {
		matched := false
		var currentShadowType string
		for _, ui := range urlsI {
			if ui.Op == uj.Op && ui.Value == uj.Value {
				matched = true
				break
			}
			st := urlShadowsURL(ui, uj)
			if st != "" {
				matched = true
				currentShadowType = st
				isIdentical = false
				break
			}
		}
		if !matched {
			return ""
		}
		if shadowType == "" && currentShadowType != "" {
			shadowType = currentShadowType
		}
	}
	if isIdentical {
		return "PrefixShadowing"
	}
	if shadowType == "" {
		return "PrefixShadowing"
	}
	return shadowType
}

func ruleShadowsRule(infoI, infoJ ruleMatchInfo) (shadowType string) {
	hostsI := getRuleHosts(infoI.Conditions)
	hostsJ := getRuleHosts(infoJ.Conditions)
	if !domainsOverlap(hostsI, hostsJ) {
		return ""
	}

	hostShadows := false
	for _, hj := range hostsJ {
		matched := false
		for _, hi := range hostsI {
			if hi.Op == hj.Op && hi.Value == hj.Value {
				matched = true
				break
			}
			if hostPatternShadows(hi, hj) {
				matched = true
				hostShadows = true
				break
			}
		}
		if !matched {
			return ""
		}
	}

	urlsI := getRuleURLs(infoI.Conditions)
	urlsJ := getRuleURLs(infoJ.Conditions)
	urlShadowType := urlPatternsCover(urlsI, urlsJ)
	if urlShadowType == "" {
		return ""
	}

	extraI := getRuleExtraConditions(infoI.Conditions)
	extraJ := getRuleExtraConditions(infoJ.Conditions)
	if !extraConditionsCover(extraI, extraJ) {
		return ""
	}

	if dslxConditionsEqual(infoI.Conditions, infoJ.Conditions) {
		return ""
	}

	if urlShadowType == "WildcardShadowing" {
		return "WildcardShadowing"
	}
	if hostShadows {
		return "PrefixShadowing"
	}
	return urlShadowType
}

func buildShadowingMessage(infoHigh, infoLow ruleMatchInfo, shadowType string, samePriority bool) string {
	urlHigh := getEffectiveURLFromConds(infoHigh.Conditions)
	if urlHigh == "" {
		urlHigh = "/"
	}
	urlLow := getEffectiveURLFromConds(infoLow.Conditions)
	if urlLow == "" {
		urlLow = "/"
	}

	hostsHigh := getRuleHosts(infoHigh.Conditions)
	displayHostHigh := "*"
	if len(hostsHigh) > 0 && hostsHigh[0].Value != "" {
		displayHostHigh = hostsHigh[0].Value
	}

	hostsLow := getRuleHosts(infoLow.Conditions)
	displayHostLow := "*"
	if len(hostsLow) > 0 && hostsLow[0].Value != "" {
		displayHostLow = hostsLow[0].Value
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
func hasEquivalentHost(condsI, condsJ []DSLXCondition) bool {
	hostsI := getRuleHosts(condsI)
	hostsJ := getRuleHosts(condsJ)
	for _, hI := range hostsI {
		for _, hJ := range hostsJ {
			if hostsEquivalent(hI, hJ) {
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
	if dslxConditionsEqual(infoI.Conditions, infoJ.Conditions) {
		urlDisplay := getEffectiveURLFromConds(infoI.Conditions)
		if urlDisplay == "" {
			urlDisplay = "/"
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
		if shadowType := ruleShadowsRule(infoI, infoJ); shadowType != "" {
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
		if hasEquivalentHost(infoI.Conditions, infoJ.Conditions) {
			if shadowTypeIJ := ruleShadowsRule(infoI, infoJ); shadowTypeIJ != "" {
				warnings = append(warnings, ConflictWarning{
					Severity:         "Warning",
					Type:             shadowTypeIJ,
					HighPriorityRule: infoI.Name,
					LowPriorityRule:  infoJ.Name,
					Message:          buildShadowingMessage(infoI, infoJ, shadowTypeIJ, true),
				})
			}
			if shadowTypeJI := ruleShadowsRule(infoJ, infoI); shadowTypeJI != "" {
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
