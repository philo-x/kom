package dynamic

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
)

func GetDynamicResource() mcp.Tool {
	return mcp.NewTool(
		"get_k8s_resource",
		mcp.WithDescription("通过集群、命名空间和名称获取Kubernetes资源详情 / Retrieve Kubernetes resource details by cluster, namespace, and name"),
		mcp.WithTitleAnnotation("Get Resource"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Description("运行资源的集群（使用空字符串表示默认集群）/ Cluster where the resources are running (use empty string for default cluster)")),
		mcp.WithString("namespace", mcp.Description("资源所在的命名空间（集群范围资源可选）/ Namespace of the resource (optional for cluster-scoped resources)")),
		mcp.WithString("name", mcp.Description("资源的名称 / Name of the resource")),
		mcp.WithString("group", mcp.Description("资源的API组 / API group of the resource")),
		mcp.WithString("version", mcp.Description("资源的API版本 / API version of the resource")),
		mcp.WithString("kind", mcp.Description("资源的类型 / Kind of the resource")),
	)
}

func GetDynamicResourceHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// 获取资源元数据
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	var item map[string]interface{}
	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx).CRD(meta.Group, meta.Version, meta.Kind).Namespace(meta.Namespace)
	if meta.Namespace == "" {
		kubectl = kubectl.AllNamespace()
	}
	err = kubectl.Name(meta.Name).RemoveManagedFields().Get(&item).Error
	if err != nil {
		return nil, fmt.Errorf("failed to get item [%s/%s] type of  [%s%s%s]: %v", meta.Namespace, meta.Name, meta.Group, meta.Version, meta.Kind, err)
	}

	if item != nil {
		slimResourceMap(item)
		kind, _ := item["kind"].(string)
		apiVersion, _ := item["apiVersion"].(string)
		if strings.EqualFold(kind, "Secret") && (apiVersion == "v1" || strings.Contains(apiVersion, "/v1")) {
			if data, ok := item["data"].(map[string]interface{}); ok {
				maskedData := make(map[string]interface{})
				for k := range data {
					maskedData[k] = "*** [Base64 Obfuscated]"
				}
				item["data"] = maskedData
			}
			if stringData, ok := item["stringData"].(map[string]interface{}); ok {
				maskedStringData := make(map[string]interface{})
				for k := range stringData {
					maskedStringData[k] = "*** [Redacted]"
				}
				item["stringData"] = maskedStringData
			}
		}
	}

	return tools.TextResult(item, meta)
}

func slimResourceMap(item map[string]interface{}) {
	if item == nil {
		return
	}

	// 1. Clean metadata
	if metadata, ok := item["metadata"].(map[string]interface{}); ok {
		delete(metadata, "managedFields")
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
			// Truncate other extremely large annotations (e.g. > 1000 characters)
			for k, v := range annotations {
				if strVal, ok := v.(string); ok && len(strVal) > 1000 {
					annotations[k] = fmt.Sprintf("... [Truncated, length %d]", len(strVal))
				}
			}
		}
	}

	// 2. Clean status to avoid noise (e.g. massive histories or conditions list)
	if status, ok := item["status"].(map[string]interface{}); ok {
		for k, v := range status {
			if sliceVal, ok := v.([]interface{}); ok {
				if len(sliceVal) > 5 {
					// Truncate to keep only the last 5 elements (most recent)
					status[k] = sliceVal[len(sliceVal)-5:]
				}
			}
			if strVal, ok := v.(string); ok && len(strVal) > 1000 {
				status[k] = fmt.Sprintf("... [Truncated, length %d]", len(strVal))
			}
		}
	}
}
