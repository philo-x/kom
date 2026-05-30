package alb2

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ALB2ServiceMappingItem defines a found routing path to the service
type ALB2ServiceMappingItem struct {
	ALB2Name     string `json:"alb2Name"`
	FrontendName string `json:"frontendName"`
	Port         int64  `json:"port"`
	Protocol     string `json:"protocol"`
	RuleName     string `json:"ruleName,omitempty"`     // Empty if it is an L4 direct proxy in Frontend spec
	Domain       string `json:"domain,omitempty"`       // For L7 rules
	URL          string `json:"url,omitempty"`          // For L7 rules
	DSL          string `json:"dsl,omitempty"`          // For L7 rules
	PortWeight   int32  `json:"portWeight"`             // Weight for this service target
	ProxyType    string `json:"proxyType"`              // "L4" or "L7"
	RuleNS       string `json:"resourceNamespace"`      // Namespace of the Rule or Frontend
}

// FindALB2ResourcesByServiceTool defines the tool schema
func FindALB2ResourcesByServiceTool() mcp.Tool {
	return mcp.NewTool(
		"find_alb2_resources_by_service",
		mcp.WithDescription("通过 K8s Service 名称反向查找其关联绑定的 ALB2 实例、Frontend 监听器及 Rule 规则。解决用户只知 Service 名字却不知对应负载均衡配置的痛点。/ Find associated ALB2, Frontend and Rule resources by K8s Service name."),
		mcp.WithTitleAnnotation("Find ALB2 Resources By Service"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行资源的集群名称 / Cluster name")),
		mcp.WithString("service_name", mcp.Required(), mcp.Description("要查找的 K8s Service 名称 / Name of the K8s Service")),
		mcp.WithString("service_namespace", mcp.Description("Service 所在的命名空间 (可选，留空则匹配全部命名空间下的同名服务) / Namespace of the K8s Service (optional)")),
	)
}

// FindALB2ResourcesByServiceHandler handles the reverse lookup requests
func FindALB2ResourcesByServiceHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	svcName := request.GetString("service_name", "")
	if svcName == "" {
		return nil, fmt.Errorf("service_name is required")
	}
	svcNS := request.GetString("service_namespace", "")

	var mappings []ALB2ServiceMappingItem

	// 1. Scan Rules (L7) in all namespaces or specific namespace
	var ruleList []*unstructured.Unstructured
	err = kom.Cluster(meta.Cluster).WithContext(ctx).
		WithCache(time.Second * 30).
		CRD("crd.alauda.io", "v1", "Rule").
		AllNamespace().
		List(&ruleList).Error

	if err != nil {
		// Fallback to v2beta1
		_ = kom.Cluster(meta.Cluster).WithContext(ctx).
			WithCache(time.Second * 30).
			CRD("crd.alauda.io", "v2beta1", "Rule").
			AllNamespace().
			List(&ruleList)
	}

	for _, rule := range ruleList {
		servicesSlice, found, _ := unstructured.NestedSlice(rule.Object, "spec", "serviceGroup", "services")
		if !found {
			continue
		}

		for _, svcRaw := range servicesSlice {
			if sMap, ok := svcRaw.(map[string]interface{}); ok {
				name, _ := sMap["name"].(string)
				namespace, _ := sMap["namespace"].(string)
				if namespace == "" {
					namespace = rule.GetNamespace()
				}

				// Check if service matches
				if name == svcName && (svcNS == "" || namespace == svcNS) {
					// Found a match in Rule
					alb2Label, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/name")
					frontendLabel, _, _ := unstructured.NestedString(rule.Object, "metadata", "labels", "alb2.cpaas.io/frontend")
					domain, _, _ := unstructured.NestedString(rule.Object, "spec", "domain")
					url, _, _ := unstructured.NestedString(rule.Object, "spec", "url")
					dsl, _, _ := unstructured.NestedString(rule.Object, "spec", "dsl")

					var weight int32
					if wVal, ok := sMap["weight"]; ok {
						switch w := wVal.(type) {
						case int64:
							weight = int32(w)
						case float64:
							weight = int32(w)
						case int:
							weight = int32(w)
						}
					}

					mappings = append(mappings, ALB2ServiceMappingItem{
						ALB2Name:     alb2Label,
						FrontendName: frontendLabel,
						RuleName:     rule.GetName(),
						Domain:       domain,
						URL:          url,
						DSL:          dsl,
						PortWeight:   weight,
						ProxyType:    "L7",
						RuleNS:       rule.GetNamespace(),
					})
				}
			}
		}
	}

	// 2. Scan Frontends (L4 direct proxy)
	var frontendList []*unstructured.Unstructured
	err = kom.Cluster(meta.Cluster).WithContext(ctx).
		WithCache(time.Second * 30).
		CRD("crd.alauda.io", "v1", "Frontend").
		AllNamespace().
		List(&frontendList).Error

	if err != nil {
		_ = kom.Cluster(meta.Cluster).WithContext(ctx).
			WithCache(time.Second * 30).
			CRD("crd.alauda.io", "v2beta1", "Frontend").
			AllNamespace().
			List(&frontendList)
	}

	for _, frontend := range frontendList {
		// Extract port and protocol
		port, _, _ := unstructured.NestedInt64(frontend.Object, "spec", "port")
		protocol, _, _ := unstructured.NestedString(frontend.Object, "spec", "protocol")

		servicesSlice, found, _ := unstructured.NestedSlice(frontend.Object, "spec", "serviceGroup", "services")
		if !found {
			continue
		}

		for _, svcRaw := range servicesSlice {
			if sMap, ok := svcRaw.(map[string]interface{}); ok {
				name, _ := sMap["name"].(string)
				namespace, _ := sMap["namespace"].(string)
				if namespace == "" {
					namespace = frontend.GetNamespace()
				}

				// Check if service matches
				if name == svcName && (svcNS == "" || namespace == svcNS) {
					alb2Label, _, _ := unstructured.NestedString(frontend.Object, "metadata", "labels", "alb2.cpaas.io/name")

					var weight int32
					if wVal, ok := sMap["weight"]; ok {
						switch w := wVal.(type) {
						case int64:
							weight = int32(w)
						case float64:
							weight = int32(w)
						case int:
							weight = int32(w)
						}
					}

					mappings = append(mappings, ALB2ServiceMappingItem{
						ALB2Name:     alb2Label,
						FrontendName: frontend.GetName(),
						Port:         port,
						Protocol:     protocol,
						PortWeight:   weight,
						ProxyType:    "L4",
						RuleNS:       frontend.GetNamespace(),
					})
				}
			}
		}
	}

	// Fill missing details for L7 rules (lookup Frontend protocol/port by name)
	for i, mapItem := range mappings {
		if mapItem.ProxyType == "L7" && mapItem.FrontendName != "" {
			// Find Frontend port/protocol
			for _, ft := range frontendList {
				if ft.GetName() == mapItem.FrontendName {
					port, _, _ := unstructured.NestedInt64(ft.Object, "spec", "port")
					protocol, _, _ := unstructured.NestedString(ft.Object, "spec", "protocol")
					mappings[i].Port = port
					mappings[i].Protocol = protocol
					break
				}
			}
		}
	}

	type FindResult struct {
		ServiceName string                   `json:"serviceName"`
		Namespace   string                   `json:"serviceNamespace,omitempty"`
		FoundCount  int                      `json:"foundCount"`
		Mappings    []ALB2ServiceMappingItem `json:"mappings"`
	}

	result := FindResult{
		ServiceName: svcName,
		Namespace:   svcNS,
		FoundCount:  len(mappings),
		Mappings:    mappings,
	}

	return tools.TextResult(result, meta)
}
