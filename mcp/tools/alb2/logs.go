package alb2

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// GetALB2ControllerLogsTool defines the tool schema
func GetALB2ControllerLogsTool() mcp.Tool {
	return mcp.NewTool(
		"get_alb2_controller_logs",
		mcp.WithDescription("自动检索与指定 ALB2 负载均衡实例关联的 Controller Pod，并提取其中包含 Error/Warning 等异常或 Reload 状态关键字的最近日志，方便诊断 Nginx Reload 失败、证书异常、配置错误等。/ Get error/warning logs for target ALB2 controller pods."),
		mcp.WithTitleAnnotation("Get ALB2 Controller Logs"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行 ALB2 的集群名称 / Cluster name")),
		mcp.WithString("alb2_name", mcp.Required(), mcp.Description("ALB2 负载均衡器实例名称 / ALB2 instance name")),
		mcp.WithNumber("tail_lines", mcp.Description("从末尾读取的日志行数 (默认 100) / Number of tail lines (default: 100)")),
	)
}

// GetALB2ControllerLogsHandler processes get_alb2_controller_logs requests
func GetALB2ControllerLogsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	alb2Name := request.GetString("alb2_name", "")
	if alb2Name == "" {
		return nil, fmt.Errorf("alb2_name is required")
	}

	tailLines := int64(request.GetInt("tail_lines", 100))

	var pods []*unstructured.Unstructured

	// 1. Try selecting pods by label alb2.cpaas.io/name in cpaas-system namespace
	err = kom.Cluster(meta.Cluster).WithContext(ctx).
		Resource(&corev1.Pod{}).
		Namespace("cpaas-system").
		WithLabelSelector(fmt.Sprintf("alb2.cpaas.io/name=%s", alb2Name)).
		List(&pods).Error

	// 2. Try selecting across all namespaces if empty
	if err != nil || len(pods) == 0 {
		_ = kom.Cluster(meta.Cluster).WithContext(ctx).
			Resource(&corev1.Pod{}).
			AllNamespace().
			WithLabelSelector(fmt.Sprintf("alb2.cpaas.io/name=%s", alb2Name)).
			List(&pods)
	}

	// 3. Fallback: Search pods containing the ALB2 name in cpaas-system namespace
	if len(pods) == 0 {
		var allPods []*unstructured.Unstructured
		errAll := kom.Cluster(meta.Cluster).WithContext(ctx).
			Resource(&corev1.Pod{}).
			Namespace("cpaas-system").
			List(&allPods).Error
		if errAll == nil {
			for _, pod := range allPods {
				if strings.Contains(pod.GetName(), alb2Name) {
					pods = append(pods, pod)
				}
			}
		}
	}

	if len(pods) == 0 {
		return tools.TextResult(fmt.Sprintf("No controller pods found for ALB2 instance: %s", alb2Name), meta)
	}

	// Diagnostic keywords for filtering
	keywords := []string{"error", "warn", "fail", "reload", "expire", "invalid", "reject", "err", "exception"}

	var logReport strings.Builder
	logReport.WriteString(fmt.Sprintf("=== ALB2 Controller Pod Logs Report for [%s] ===\n", alb2Name))
	logReport.WriteString(fmt.Sprintf("Found %d Pods. Retreiving last %d lines filtered for warning/error keywords.\n\n", len(pods), tailLines))

	for _, pod := range pods {
		podName := pod.GetName()
		podNS := pod.GetNamespace()

		logReport.WriteString(fmt.Sprintf("--- Pod: %s/%s ---\n", podNS, podName))

		var stream io.ReadCloser
		opt := &corev1.PodLogOptions{
			TailLines: ptr(tailLines),
		}

		errLogs := kom.Cluster(meta.Cluster).WithContext(ctx).
			Namespace(podNS).
			Name(podName).
			Ctl().Pod().
			GetLogs(&stream, opt).Error

		if errLogs != nil {
			logReport.WriteString(fmt.Sprintf("[Error getting logs: %v]\n\n", errLogs))
			continue
		}

		logsBytes, errRead := io.ReadAll(stream)
		stream.Close()
		if errRead != nil {
			logReport.WriteString(fmt.Sprintf("[Error reading log stream: %v]\n\n", errRead))
			continue
		}

		// Filter lines
		logContent := string(logsBytes)
		lines := strings.Split(logContent, "\n")
		filteredCount := 0

		for _, line := range lines {
			if line == "" {
				continue
			}

			// Check for keywords
			lowerLine := strings.ToLower(line)
			match := false
			for _, kw := range keywords {
				if strings.Contains(lowerLine, kw) {
					match = true
					break
				}
			}

			if match {
				logReport.WriteString(line + "\n")
				filteredCount++
			}
		}

		if filteredCount == 0 {
			logReport.WriteString("[No diagnostic warnings/errors found in the tail lines]\n")
		} else {
			logReport.WriteString(fmt.Sprintf("[Filtered %d relevant rows]\n", filteredCount))
		}
		logReport.WriteString("\n")
	}

	return tools.TextResult(logReport.String(), meta)
}

func ptr[T any](v T) *T {
	return &v
}
