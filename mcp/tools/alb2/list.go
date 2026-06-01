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

// ALB2ResourceItem defines the returned structure for an ALB2 instance
type ALB2ResourceItem struct {
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	APIVersion        string    `json:"apiVersion"`
	CreationTimestamp time.Time `json:"creationTimestamp"`
	Address           string    `json:"address,omitempty"`
	BindIP            string    `json:"bindIP,omitempty"`
	VIP               string    `json:"vip,omitempty"`
	StatusIPs         []string  `json:"statusIPs,omitempty"`
	Ready             bool      `json:"ready"`
	State             string    `json:"state,omitempty"`
	Phase             string    `json:"phase,omitempty"`
	Message           string    `json:"message,omitempty"`
}

// ListALB2ResourcesTool defines the tool schema with pagination support
func ListALB2ResourcesTool() mcp.Tool {
	return mcp.NewTool(
		"list_alb2_resources",
		mcp.WithDescription("列出指定 Namespace 下的所有 ALB2 负载均衡实例、它们的健康状态及其 IP/VIP 绑定关系（支持分页）。/ List all ALB2 instances, their health status and IP/VIP bindings in a namespace (paginated)."),
		mcp.WithTitleAnnotation("List ALB2 Resources"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行 ALB2 的集群名称 / Cluster name")),
		mcp.WithString("namespace", mcp.Description("ALB2 所在的命名空间 (默认 cpaas-system) / Namespace of ALB2 instances (default: cpaas-system)")),
		mcp.WithNumber("page", mcp.Description("页码，从1开始（默认1）/ Page number, starting from 1 (default 1)")),
		mcp.WithNumber("pageSize", mcp.Description("每页返回的资源数量（默认10，最大300）/ Number of resources per page (default 10, max 300)")),
	)
}

// ListALB2ResourcesHandler processes list_alb2_resources request with pagination
func ListALB2ResourcesHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Parse basic cluster & namespace arguments
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	// Default to cpaas-system if namespace is empty
	ns := request.GetString("namespace", "cpaas-system")
	meta.Namespace = ns

	// Parse pagination parameters
	page, pageSize, offset := tools.ParsePagination(request)

	var list []*unstructured.Unstructured

	// Try querying crd.alauda.io/v1 first (Production Version)
	err = kom.Cluster(meta.Cluster).WithContext(ctx).
		WithCache(time.Second*30).
		CRD("crd.alauda.io", "v1", "ALB2").
		Namespace(meta.Namespace).
		List(&list).Error

	if err != nil {
		// Fall back to crd.alauda.io/v2beta1
		err = kom.Cluster(meta.Cluster).WithContext(ctx).
			WithCache(time.Second*30).
			CRD("crd.alauda.io", "v2beta1", "ALB2").
			Namespace(meta.Namespace).
			List(&list).Error
		if err != nil {
			return nil, fmt.Errorf("failed to list ALB2 instances using v1 or v2beta1: %v", err)
		}
	}

	total := int64(len(list))
	start := offset
	if start > len(list) {
		start = len(list)
	}
	end := offset + pageSize
	if end > len(list) {
		end = len(list)
	}
	slicedList := list[start:end]

	var results []ALB2ResourceItem
	for _, item := range slicedList {
		// Extract address fields
		address, _, _ := unstructured.NestedString(item.Object, "spec", "address")
		bindIP, _, _ := unstructured.NestedString(item.Object, "spec", "bindIP")
		statusBindIP, _, _ := unstructured.NestedString(item.Object, "status", "bindIP")
		statusVIP, _, _ := unstructured.NestedString(item.Object, "status", "vip")

		// Extract status arrays
		var statusIPs []string
		if ipv4Slice, found, _ := unstructured.NestedSlice(item.Object, "status", "detail", "address", "ipv4"); found {
			for _, ip := range ipv4Slice {
				if ipStr, ok := ip.(string); ok {
					statusIPs = append(statusIPs, ipStr)
				}
			}
		}
		if ipv6Slice, found, _ := unstructured.NestedSlice(item.Object, "status", "detail", "address", "ipv6"); found {
			for _, ip := range ipv6Slice {
				if ipStr, ok := ip.(string); ok {
					statusIPs = append(statusIPs, ipStr)
				}
			}
		}

		// Health states
		ok, okFound, _ := unstructured.NestedBool(item.Object, "status", "detail", "ok")
		state, _, _ := unstructured.NestedString(item.Object, "status", "state")
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		msg, _, _ := unstructured.NestedString(item.Object, "status", "detail", "msg")

		// Determine readiness
		ready := false
		if okFound {
			ready = ok
		} else if state != "" {
			ready = (state == "running" || state == "ready" || state == "Ready" || state == "Running")
		} else if phase != "" {
			ready = (phase == "Running" || phase == "ready" || phase == "Ready")
		} else {
			// If status fields are completely missing, check if it has creation timestamp (it's at least created)
			ready = true
		}

		results = append(results, ALB2ResourceItem{
			Name:              item.GetName(),
			Namespace:         item.GetNamespace(),
			APIVersion:        item.GetAPIVersion(),
			CreationTimestamp: item.GetCreationTimestamp().Time,
			Address:           address,
			BindIP:            bindIP,
			VIP:               statusVIP,
			StatusIPs:         statusIPs,
			Ready:             ready,
			State:             state,
			Phase:             phase,
			Message:           msg,
		})
		if statusBindIP != "" && statusBindIP != bindIP {
			// fallback/addition
			if results[len(results)-1].BindIP == "" {
				results[len(results)-1].BindIP = statusBindIP
			}
		}
	}

	paginated := tools.BuildPaginatedResult(results, total, page, pageSize)
	return tools.TextResult(paginated, meta)
}
