package node

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ListNode() mcp.Tool {
	return mcp.NewTool(
		"list_k8s_node",
		mcp.WithDescription("获取Node列表 (类似命令 kubectl get node)。返回结果为分页格式，包含 items（当前页数据）、total（总数）、page（当前页码）、pageSize（每页大小）、totalPages（总页数）。如需获取更多数据，请增大 page 参数值。/ List nodes with pagination. Response includes items, total, page, pageSize, totalPages."),
		mcp.WithTitleAnnotation("List Nodes"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Description("Node所在集群（使用空字符串表示默认集群）")),
		mcp.WithNumber("page", mcp.Description("页码，从1开始（默认1）/ Page number, starting from 1 (default 1)")),
		mcp.WithNumber("pageSize", mcp.Description("每页返回的资源数量（默认10，最大500）/ Number of resources per page (default 10, max 500)")),
	)
}

func ListNodeHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	// 获取资源元数据
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	// 解析分页参数
	page, pageSize, offset := tools.ParsePagination(request)

	// 获取资源列表
	var list []*unstructured.Unstructured
	var total int64
	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx).
		Resource(&v1.Node{}).RemoveManagedFields()

	err = kubectl.
		FillTotalCount(&total).
		Limit(pageSize).
		Offset(offset).
		List(&list).Error
	if err != nil {
		return nil, fmt.Errorf("failed to list items type of [%s%s%s]: %v", meta.Group, meta.Version, meta.Kind, err)
	}

	// 提取name信息
	var result []map[string]string
	for _, item := range list {
		ret := map[string]string{
			"name": item.GetName(),
		}

		result = append(result, ret)
	}

	// 构造分页返回结果
	paginated := tools.BuildPaginatedResult(result, total, page, pageSize)
	return tools.TextResult(paginated, meta)
}
