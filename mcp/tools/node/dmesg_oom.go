package node

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
)

func GetNodeDmesgOOMTool() mcp.Tool {
	return mcp.NewTool(
		"get_k8s_node_dmesg_oom",
		mcp.WithDescription("扫描节点内核日志（dmesg/syslog/messages）中是否有内存溢出（OOM-killer）记录。 / Scan node kernel logs for Out-of-Memory (OOM-killer) events."),
		mcp.WithTitleAnnotation("Get Node OOM Logs"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("集群名称/Cluster name")),
		mcp.WithString("node_name", mcp.Required(), mcp.Description("节点名称 / Node name")),
	)
}

func GetNodeDmesgOOMHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	nodeName := request.GetString("node_name", "")
	if nodeName == "" {
		return nil, fmt.Errorf("node_name is required")
	}

	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx)
	clientset := kubectl.Client()
	if clientset == nil {
		return nil, fmt.Errorf("failed to get kubernetes clientset")
	}

	// Try querying /logs/dmesg first
	res := clientset.CoreV1().RESTClient().Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("logs/dmesg")

	var logData []byte
	logSource := "dmesg"
	logData, err = res.DoRaw(ctx)
	if err != nil {
		// Fallback to syslog
		resSyslog := clientset.CoreV1().RESTClient().Get().
			Resource("nodes").
			Name(nodeName).
			SubResource("proxy").
			Suffix("logs/syslog")
		logData, err = resSyslog.DoRaw(ctx)
		logSource = "syslog"
	}
	if err != nil {
		// Fallback to messages
		resMsg := clientset.CoreV1().RESTClient().Get().
			Resource("nodes").
			Name(nodeName).
			SubResource("proxy").
			Suffix("logs/messages")
		logData, err = resMsg.DoRaw(ctx)
		logSource = "messages"
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get dmesg or fallback log files: %v. Please make sure Kubelet EnableSystemLogHandler is enabled on node %s.", err, nodeName)
	}

	// Scan log data for OOM signatures
	scanner := bufio.NewScanner(strings.NewReader(string(logData)))
	var oomLines []string
	oomKeywords := []string{"out of memory", "oom-killer", "killed process", "oom_kill", "invoked oom-killer"}

	for scanner.Scan() {
		line := scanner.Text()
		lowerLine := strings.ToLower(line)
		for _, kw := range oomKeywords {
			if strings.Contains(lowerLine, kw) {
				oomLines = append(oomLines, line)
				break
			}
		}
	}

	if len(oomLines) == 0 {
		return tools.TextResult(fmt.Sprintf("No kernel OOM-killer signatures found in node [%s] %s logs.", nodeName, logSource), meta)
	}

	// Limit output size to prevent context overflow
	if len(oomLines) > 200 {
		oomLines = oomLines[len(oomLines)-200:]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d OOM-killer logs in node [%s] %s logs:\n", len(oomLines), nodeName, logSource))
	for _, oomLine := range oomLines {
		sb.WriteString(oomLine + "\n")
	}

	return tools.TextResult(sb.String(), meta)
}
