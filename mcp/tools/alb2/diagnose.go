package alb2

import (
	"context"
	"fmt"
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

// DiagnoseALB2RuleConflictTool defines the tool schema
func DiagnoseALB2RuleConflictTool() mcp.Tool {
	return mcp.NewTool(
		"diagnose_alb2_rule_conflict",
		mcp.WithDescription("静态分析指定 Frontend 端口下是否存在路由规则优先级冲突、宽泛路径抢占精确路径（例如高优先级 rule-wildcard `/` 抢占低优先级 rule-specific `/manager`）等异常现象。/ Statically analyze routing rules under a Frontend for priority conflicts or shadowing issues."),
		mcp.WithTitleAnnotation("Diagnose ALB2 Rule Conflict"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行 ALB2 的集群名称 / Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("ALB2 所在的命名空间 / Namespace")),
		mcp.WithString("frontend_name", mcp.Required(), mcp.Description("进行诊断的 Frontend 监听器名称，例如 center5-100-115-99-170-00080 / Target Frontend name")),
	)
}

// DiagnoseALB2RuleConflictHandler processes diagnose_alb2_rule_conflict request
func DiagnoseALB2RuleConflictHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	frontendName := request.GetString("frontend_name", "")
	if frontendName == "" {
		return nil, fmt.Errorf("frontend_name is required")
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

	// Filter rules belonging to the specified frontend
	var matchedRules []*unstructured.Unstructured
	for _, rule := range ruleList {
		frontendLabel, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/frontend")
		if frontendLabel == frontendName {
			matchedRules = append(matchedRules, rule)
		}
	}

	// Sort rules by priority (ascending)
	sort.Slice(matchedRules, func(i, j int) bool {
		return getRulePriority(matchedRules[i]) < getRulePriority(matchedRules[j])
	})

	var warnings []ConflictWarning

	// Compare each rule pair (i, j) where i has a higher priority than j (i < j)
	for i := 0; i < len(matchedRules); i++ {
		rI := matchedRules[i]
		pI := getRulePriority(rI)
		domI, _, _ := unstructured.NestedString(rI.Object, "spec", "domain")
		urlI, _, _ := unstructured.NestedString(rI.Object, "spec", "url")
		dslI, _, _ := unstructured.NestedString(rI.Object, "spec", "dsl")

		for j := i + 1; j < len(matchedRules); j++ {
			rJ := matchedRules[j]
			pJ := getRulePriority(rJ)
			domJ, _, _ := unstructured.NestedString(rJ.Object, "spec", "domain")
			urlJ, _, _ := unstructured.NestedString(rJ.Object, "spec", "url")
			dslJ, _, _ := unstructured.NestedString(rJ.Object, "spec", "dsl")

			// Check domain compatibility
			domainsOverlap := false
			if domI == "" || domI == "*" || domJ == "" || domJ == "*" {
				domainsOverlap = true
			} else if domI == domJ {
				domainsOverlap = true
			}

			if !domainsOverlap {
				continue
			}

			// 1. Identical path/DSL duplicate matches
			if urlI == urlJ && dslI == dslJ {
				warnings = append(warnings, ConflictWarning{
					Severity:         "Critical",
					Type:             "DuplicateMatching",
					HighPriorityRule: rI.GetName(),
					LowPriorityRule:  rJ.GetName(),
					Message:          fmt.Sprintf("⚠️ 关键冲突：Rule [%s] (优先级: %d) 与 Rule [%s] (优先级: %d) 拥有完全相同的匹配路径 (%s)。低优先级的规则将永远无法被访问。", rI.GetName(), pI, rJ.GetName(), pJ, urlI),
				})
				continue
			}

			// 2. Wildcard shadow (e.g. "/" or empty path shadows specific paths)
			if (urlI == "/" || urlI == "") && urlJ != "/" && urlJ != "" && dslI == "" {
				warnings = append(warnings, ConflictWarning{
					Severity:         "Warning",
					Type:             "WildcardShadowing",
					HighPriorityRule: rI.GetName(),
					LowPriorityRule:  rJ.GetName(),
					Message:          fmt.Sprintf("⚠️ 警告：高优先级规则 [%s] (优先级: %d, 路径: `%s`) 匹配了全部流量，这会导致低优先级规则 [%s] (优先级: %d, 路径: `%s`) 被强行拦截屏蔽。", rI.GetName(), pI, urlI, rJ.GetName(), pJ, urlJ),
				})
				continue
			}

			// 3. Path prefix shadow (e.g. "/manager" shadows "/manager/specific")
			if urlI != "" && urlJ != "" && urlI != "/" && strings.HasPrefix(urlJ, urlI) && urlI != urlJ {
				// Verify prefix boundaries (e.g., /manager shadows /manager/abc but not /manager-etc)
				isSubPath := false
				if strings.HasSuffix(urlI, "/") {
					isSubPath = true
				} else {
					boundaryChar := urlJ[len(urlI)]
					if boundaryChar == '/' || boundaryChar == '?' || boundaryChar == '#' {
						isSubPath = true
					}
				}

				if isSubPath && dslI == "" {
					warnings = append(warnings, ConflictWarning{
						Severity:         "Warning",
						Type:             "PrefixShadowing",
						HighPriorityRule: rI.GetName(),
						LowPriorityRule:  rJ.GetName(),
						Message:          fmt.Sprintf("⚠️ 警告：前缀路径规则 [%s] (优先级: %d, 路径: `%s`) evaluation 优于具体路径规则 [%s] (优先级: %d, 路径: `%s`)，将劫持该子路径的所有请求。", rI.GetName(), pI, urlI, rJ.GetName(), pJ, urlJ),
					})
				}
			}
		}
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
