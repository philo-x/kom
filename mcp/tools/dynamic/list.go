package dynamic

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ListDynamicResource() mcp.Tool {
	return mcp.NewTool(
		"list_k8s_resource",
		mcp.WithDescription("按集群和资源类型列出Kubernetes资源，获取列表。返回结果为分页格式，包含 items（当前页数据）、total（总数）、page（当前页码）、pageSize（每页大小）、totalPages（总页数）。如需获取更多数据，请增大 page 参数值。/ List Kubernetes resources by cluster and resource type with pagination. Response includes items, total, page, pageSize, totalPages."),
		mcp.WithTitleAnnotation("List Resources"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行资源的集群/Cluster where the resources are running")),
		mcp.WithString("namespace", mcp.Description("资源所在的命名空间（集群范围资源可选）/ Namespace of the resources (optional for cluster-scoped resources)")),
		mcp.WithString("group", mcp.Description("资源的API组 / API group of the resource")),
		mcp.WithString("version", mcp.Description("资源的API版本 / API version of the resource")),
		mcp.WithString("kind", mcp.Description("资源的类型 / Kind of the resource")),
		mcp.WithString("labelSelector", mcp.Description("用于过滤资源的标签选择器（例如：app=k8m）/ Label selector to filter resources (e.g. app=k8m)")),
		mcp.WithString("fieldSelector", mcp.Description("用于过滤资源的字段选择器（例如：metadata.name=test-deploy）/ Field selector to filter resources (e.g. metadata.name=test-deploy)")),
		mcp.WithNumber("page", mcp.Description("页码，从1开始（默认1）/ Page number, starting from 1 (default 1)")),
		mcp.WithNumber("pageSize", mcp.Description("每页返回的资源数量（默认10，最大300）/ Number of resources per page (default 10, max 300)")),
	)
}

func ListDynamicResourceHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	// 获取资源元数据
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	// 获取标签选择器和字段选择器
	labelSelector := request.GetString("labelSelector", "")
	fieldSelector := request.GetString("fieldSelector", "")

	// 解析分页参数
	page, pageSize, offset := tools.ParsePagination(request)

	// 获取资源列表
	var list []*unstructured.Unstructured
	var total int64
	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx).CRD(meta.Group, meta.Version, meta.Kind).Namespace(meta.Namespace).RemoveManagedFields()
	if meta.Namespace == "" {
		kubectl = kubectl.AllNamespace()
	}
	if labelSelector != "" {
		kubectl = kubectl.WithLabelSelector(labelSelector)
	}
	if fieldSelector != "" {
		kubectl = kubectl.WithFieldSelector(fieldSelector)
	}
	err = kubectl.
		FillTotalCount(&total).
		Limit(pageSize).
		Offset(offset).
		List(&list).Error
	if err != nil {
		return nil, fmt.Errorf("failed to list items type of [%s%s%s]: %v", meta.Group, meta.Version, meta.Kind, err)
	}

	// 提取name和namespace信息
	var result []map[string]string
	for _, item := range list {
		ret := map[string]string{
			"name": item.GetName(),
		}
		if item.GetNamespace() != "" {
			ret["namespace"] = item.GetNamespace()
		}

		result = append(result, ret)
	}

	// 构造分页返回结果
	paginated := tools.BuildPaginatedResult(result, total, page, pageSize)
	return tools.TextResult(paginated, meta)
}
