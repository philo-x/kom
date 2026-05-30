package alb2

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ALB2RuleBackendDiagnostics diagnostics details for rule backends
type ALB2RuleBackendDiagnostics struct {
	ServiceName    string `json:"serviceName"`
	Namespace      string `json:"namespace"`
	Port           int32  `json:"port"`
	Weight         int32  `json:"weight"`
	ServiceExists  bool   `json:"serviceExists"`
	EndpointsCount int    `json:"endpointsCount"`
}

// ALB2RoutingRuleItem struct defining a routing rule configuration with health diagnostics
type ALB2RoutingRuleItem struct {
	Name            string                       `json:"name"`
	Priority        int64                        `json:"priority"`
	Domain          string                       `json:"domain"`
	DSL             string                       `json:"dsl,omitempty"`
	URL             string                       `json:"url,omitempty"`
	BackendProtocol string                       `json:"backendProtocol,omitempty"`
	Description     string                       `json:"description,omitempty"`
	Backends        []ALB2RuleBackendDiagnostics `json:"backends"`
}

// ListALB2RoutingRulesTool defines the tool metadata with pagination support
func ListALB2RoutingRulesTool() mcp.Tool {
	return mcp.NewTool(
		"list_alb2_routing_rules",
		mcp.WithDescription("按分页列出特定 ALB2 实例下所有的路由规则，按优先级（priority 升序）全局排序返回。输出附带后端服务（Service/Endpoints）的存在性及健康诊断数据。/ List all routing rules under a specific ALB2 instance sorted by priority with pagination, with backend service diagnostics."),
		mcp.WithTitleAnnotation("List ALB2 Routing Rules"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行 ALB2 的集群名称 / Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("ALB2 所在的命名空间 / Namespace")),
		mcp.WithString("alb2_name", mcp.Required(), mcp.Description("过滤特定的 ALB2 实例名称 (必填) / Filter by ALB2 name (Required)")),
		mcp.WithString("frontend_name", mcp.Description("过滤特定的 Frontend 监听器名称，例如 center5-100-115-99-170-00080 / Filter by Frontend name")),
		mcp.WithNumber("page", mcp.Description("页码，从1开始（默认1）/ Page number, starting from 1 (default 1)")),
		mcp.WithNumber("pageSize", mcp.Description("每页返回的资源数量（默认10，最大500）/ Number of resources per page (default 10, max 500)")),
	)
}

// ListALB2RoutingRulesHandler processes list_alb2_routing_rules requests
func ListALB2RoutingRulesHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	alb2Name := request.GetString("alb2_name", "")
	if alb2Name == "" {
		return nil, fmt.Errorf("alb2_name is required")
	}
	frontendName := request.GetString("frontend_name", "")

	// Parse pagination parameters
	page, pageSize, offset := tools.ParsePagination(request)

	var ruleList []*unstructured.Unstructured

	// Try querying rules under crd.alauda.io/v1 first (Production Version)
	err = kom.Cluster(meta.Cluster).WithContext(ctx).
		WithCache(time.Second*30).
		CRD("crd.alauda.io", "v1", "Rule").
		Namespace(meta.Namespace).
		List(&ruleList).Error

	if err != nil {
		// Fall back to v2beta1
		err = kom.Cluster(meta.Cluster).WithContext(ctx).
			WithCache(time.Second*30).
			CRD("crd.alauda.io", "v2beta1", "Rule").
			Namespace(meta.Namespace).
			List(&ruleList).Error
		if err != nil {
			return nil, fmt.Errorf("failed to list Rule resources using v1 or v2beta1: %v", err)
		}
	}

	// Filter rules
	var matchedRules []*unstructured.Unstructured
	for _, rule := range ruleList {
		frontendLabel, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/frontend")
		alb2Label, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/name")

		match := true
		if frontendName != "" && frontendLabel != frontendName {
			match = false
		}
		if alb2Label != alb2Name {
			match = false
		}
		if match {
			matchedRules = append(matchedRules, rule)
		}
	}

	// Sort rules by priority globally (ascending)
	sort.Slice(matchedRules, func(i, j int) bool {
		return getRulePriority(matchedRules[i]) < getRulePriority(matchedRules[j])
	})

	// Total count of matched rules
	total := int64(len(matchedRules))

	// Slice rules according to page parameters (In-memory paging to maintain correct global priority order)
	start := offset
	if start > len(matchedRules) {
		start = len(matchedRules)
	}
	end := offset + pageSize
	if end > len(matchedRules) {
		end = len(matchedRules)
	}
	slicedRules := matchedRules[start:end]

	var results []ALB2RoutingRuleItem
	for _, rule := range slicedRules {
		priority := getRulePriority(rule)
		domain, _, _ := unstructured.NestedString(rule.Object, "spec", "domain")
		dsl, _, _ := unstructured.NestedString(rule.Object, "spec", "dsl")
		url, _, _ := unstructured.NestedString(rule.Object, "spec", "url")
		backendProtocol, _, _ := unstructured.NestedString(rule.Object, "spec", "backendProtocol")
		description, _, _ := unstructured.NestedString(rule.Object, "spec", "description")

		var backends []ALB2RuleBackendDiagnostics

		// Extract services
		servicesSlice, found, _ := unstructured.NestedSlice(rule.Object, "spec", "serviceGroup", "services")
		if found {
			for _, itemRaw := range servicesSlice {
				if sMap, ok := itemRaw.(map[string]interface{}); ok {
					svcName, _ := sMap["name"].(string)
					svcNamespace, _ := sMap["namespace"].(string)
					if svcNamespace == "" {
						svcNamespace = rule.GetNamespace()
					}

					var svcPort int32
					if portVal, ok := sMap["port"]; ok {
						switch p := portVal.(type) {
						case int64:
							svcPort = int32(p)
						case float64:
							svcPort = int32(p)
						case int:
							svcPort = int32(p)
						}
					}

					var svcWeight int32
					if weightVal, ok := sMap["weight"]; ok {
						switch w := weightVal.(type) {
						case int64:
							svcWeight = int32(w)
						case float64:
							svcWeight = int32(w)
						case int:
							svcWeight = int32(w)
						}
					}

					// Diagnoses
					var svc corev1.Service
					errSvc := kom.Cluster(meta.Cluster).WithContext(ctx).
						WithCache(time.Second * 15).
						Resource(&corev1.Service{}).
						Namespace(svcNamespace).
						Name(svcName).
						Get(&svc).Error
					svcExists := (errSvc == nil)

					var ep corev1.Endpoints
					errEp := kom.Cluster(meta.Cluster).WithContext(ctx).
						WithCache(time.Second * 15).
						Resource(&corev1.Endpoints{}).
						Namespace(svcNamespace).
						Name(svcName).
						Get(&ep).Error

					endpointsCount := 0
					if errEp == nil {
						for _, subset := range ep.Subsets {
							endpointsCount += len(subset.Addresses)
						}
					}

					backends = append(backends, ALB2RuleBackendDiagnostics{
						ServiceName:    svcName,
						Namespace:      svcNamespace,
						Port:           svcPort,
						Weight:         svcWeight,
						ServiceExists:  svcExists,
						EndpointsCount: endpointsCount,
					})
				}
			}
		}

		results = append(results, ALB2RoutingRuleItem{
			Name:            rule.GetName(),
			Priority:        priority,
			Domain:          domain,
			DSL:             dsl,
			URL:             url,
			BackendProtocol: backendProtocol,
			Description:     description,
			Backends:        backends,
		})
	}

	paginated := tools.BuildPaginatedResult(results, total, page, pageSize)
	return tools.TextResult(paginated, meta)
}

func getRulePriority(obj *unstructured.Unstructured) int64 {
	p, found, _ := unstructured.NestedInt64(obj.Object, "spec", "priority")
	if !found {
		return 99999
	}
	return p
}
