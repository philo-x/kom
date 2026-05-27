package tools

// ResourceMetadata 封装资源的元数据信息
type ResourceMetadata struct {
	Cluster   string
	Namespace string
	Name      string
	Group     string
	Version   string
	Kind      string
}

type ResourceInfo struct {
	Group      string
	Version    string
	Kind       string
	Namespaced bool
}

// PaginatedResult 统一的分页返回结构
type PaginatedResult struct {
	Items      interface{} `json:"items"`      // 当前页的数据列表
	Total      int64       `json:"total"`      // 符合条件的资源总数
	Page       int         `json:"page"`       // 当前页码（从1开始）
	PageSize   int         `json:"pageSize"`   // 每页大小
	TotalPages int         `json:"totalPages"` // 总页数
}
