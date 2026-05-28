package node

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
)

func GetNodeSystemLogsTool() mcp.Tool {
	return mcp.NewTool(
		"get_k8s_node_system_logs",
		mcp.WithDescription("获取指定节点的系统组件日志（如 kubelet, containerd 等）。从 Kubelet logs 代理接口拉取。 / Retrieve system service logs (kubelet, containerd, etc.) from Kubelet logs proxy."),
		mcp.WithTitleAnnotation("Get Node System Logs"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("集群名称/Cluster name")),
		mcp.WithString("node_name", mcp.Required(), mcp.Description("节点名称 / Node name")),
		mcp.WithString("service", mcp.Required(), mcp.Description("服务组件名称 (如 kubelet, containerd, docker) / Service unit name (e.g. kubelet, containerd, docker)")),
		mcp.WithNumber("tail_lines", mcp.Description("返回最后的日志行数（默认 100）/ Number of tail log lines (default 100)")),
	)
}

func GetNodeSystemLogsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	nodeName := request.GetString("node_name", "")
	service := request.GetString("service", "")
	tailLines := request.GetInt("tail_lines", 100)
	if tailLines <= 0 {
		tailLines = 100
	}

	if nodeName == "" || service == "" {
		return nil, fmt.Errorf("node_name and service are required")
	}

	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx)
	clientset := kubectl.Client()
	if clientset == nil {
		return nil, fmt.Errorf("failed to get kubernetes clientset")
	}

	// Try querying /logs/journal?unit={service}&tail={tailLines}
	res := clientset.CoreV1().RESTClient().Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("logs/journal").
		Param("unit", service).
		Param("tail", fmt.Sprintf("%d", tailLines))

	data, err := res.DoRaw(ctx)
	if err == nil {
		return tools.TextResult(string(data), meta)
	}

	// Fallback 1: Try reading syslog if journald fails
	resFallback := clientset.CoreV1().RESTClient().Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("logs/syslog")

	data, err = resFallback.DoRaw(ctx)
	if err == nil {
		// Return syslog but filter or trim if it is too large
		logStr := string(data)
		if len(logStr) > 50000 {
			logStr = "[Truncated due to size] ...\n" + logStr[len(logStr)-50000:]
		}
		return tools.TextResult(fmt.Sprintf("Failed to get journald logs (Kubelet system log proxy disabled or systemd missing). Falling back to syslog file:\n%s", logStr), meta)
	}

	// Fallback 2: Try reading messages
	resFallback2 := clientset.CoreV1().RESTClient().Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("logs/messages")

	data, err = resFallback2.DoRaw(ctx)
	if err == nil {
		logStr := string(data)
		if len(logStr) > 50000 {
			logStr = "[Truncated due to size] ...\n" + logStr[len(logStr)-50000:]
		}
		return tools.TextResult(fmt.Sprintf("Failed to get journald logs. Falling back to messages file:\n%s", logStr), meta)
	}

	return nil, fmt.Errorf("failed to get node system logs from journal, syslog, or messages: %v. Please make sure Kubelet EnableSystemLogHandler is enabled on target node.", err)
}
