package dynamic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func GetResourceMetricsHistoryTool() mcp.Tool {
	return mcp.NewTool(
		"get_k8s_resource_metrics_history",
		mcp.WithDescription("获取Pod或Node资源（CPU/Memory）的Prometheus历史指标。 / Get historical CPU/Memory metrics for a Pod or Node from Prometheus."),
		mcp.WithTitleAnnotation("Get Metrics History"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Description("集群名称（使用空字符串表示默认集群）/ Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("资源所在的命名空间（如果是Node则填空或default）/ Namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("资源名称 (Pod名或Node名) / Resource name")),
		mcp.WithString("kind", mcp.Required(), mcp.Description("资源类型 (Pod 或 Node) / Resource kind")),
		mcp.WithString("metric_type", mcp.Required(), mcp.Description("指标类型 (cpu 或 memory) / Metric type")),
		mcp.WithString("prometheus_namespace", mcp.Description("Prometheus所在命名空间（可选，自动探测）/ Prometheus namespace (optional)")),
		mcp.WithString("prometheus_service", mcp.Description("Prometheus服务名称（可选，自动探测）/ Prometheus service (optional)")),
		mcp.WithNumber("duration_minutes", mcp.Description("查询过去多少分钟的历史（默认 60）/ Query range in minutes (default 60)")),
	)
}

func GetResourceMetricsHistoryHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	kind := request.GetString("kind", "")
	name := request.GetString("name", "")
	metricType := request.GetString("metric_type", "")
	promNs := request.GetString("prometheus_namespace", "")
	promSvc := request.GetString("prometheus_service", "")
	durationMinutes := request.GetInt("duration_minutes", 60)
	if durationMinutes <= 0 {
		durationMinutes = 60
	}

	if name == "" || kind == "" || metricType == "" {
		return nil, fmt.Errorf("name, kind, and metric_type are required")
	}

	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx)
	clientset := kubectl.Client()
	if clientset == nil {
		return nil, fmt.Errorf("failed to get kubernetes clientset")
	}

	// Step 1: Autodetect Prometheus if namespace or service is missing
	if promNs == "" || promSvc == "" {
		detectedNs, detectedSvc := autodetectPrometheus(ctx, clientset)
		if promNs == "" {
			promNs = detectedNs
		}
		if promSvc == "" {
			promSvc = detectedSvc
		}
	}

	// Step 2: Construct PromQL query based on kind and metric type
	var queryExpr string
	if strings.EqualFold(kind, "Pod") {
		if strings.EqualFold(metricType, "cpu") {
			// Pod CPU usage in cores (5m rate)
			queryExpr = fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace="%s", pod="%s", container!=""}[5m])) by (container)`, meta.Namespace, name)
		} else if strings.EqualFold(metricType, "memory") {
			// Pod memory working set bytes
			queryExpr = fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace="%s", pod="%s", container!=""}) by (container)`, meta.Namespace, name)
		} else {
			return nil, fmt.Errorf("unsupported metric type: %s, only 'cpu' or 'memory' allowed", metricType)
		}
	} else if strings.EqualFold(kind, "Node") {
		if strings.EqualFold(metricType, "cpu") {
			// Node CPU usage percent (non-idle)
			queryExpr = fmt.Sprintf(`sum(rate(node_cpu_seconds_total{mode!="idle", instance=~".*%s.*"}[5m]))`, name)
		} else if strings.EqualFold(metricType, "memory") {
			// Node memory usage (Total - Available)
			queryExpr = fmt.Sprintf(`node_memory_MemTotal_bytes{instance=~".*%s.*"} - node_memory_MemAvailable_bytes{instance=~".*%s.*"}`, name, name)
		} else {
			return nil, fmt.Errorf("unsupported metric type: %s, only 'cpu' or 'memory' allowed", metricType)
		}
	} else {
		return nil, fmt.Errorf("unsupported kind: %s, only 'Pod' or 'Node' allowed", kind)
	}

	// Step 3: Run Prometheus query
	promService := kubectl.Prometheus()
	promClient := promService.WithInClusterEndpoint(promNs, promSvc)
	if promClient == nil {
		return nil, fmt.Errorf("failed to initialize Prometheus client for %s/%s", promNs, promSvc)
	}

	end := time.Now()
	start := end.Add(-time.Duration(durationMinutes) * time.Minute)
	step := 1 * time.Minute
	if durationMinutes > 180 {
		step = 5 * time.Minute
	}

	result, err := promClient.Expr(queryExpr).QueryRange(start, end, step)
	if err != nil {
		return nil, fmt.Errorf("failed to query Prometheus service: %v. (Address: %s/%s, Query: %s)", err, promNs, promSvc, queryExpr)
	}

	matrix := result.AsMatrix()
	if len(matrix) == 0 {
		return tools.TextResult(fmt.Sprintf("No historical metrics found for %s %s. Query used: %s", kind, name, queryExpr), meta)
	}

	// Step 4: Format metrics data as text output
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Historical %s metrics for %s %s (from Prometheus %s/%s):\n", strings.ToUpper(metricType), kind, name, promNs, promSvc))
	for _, series := range matrix {
		sb.WriteString("\nSeries labels:\n")
		for k, v := range series.Metric {
			sb.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
		sb.WriteString("Data points (Time -> Value):\n")
		// Limit data points to avoid too much output (max 60 points)
		samples := series.Samples
		if len(samples) > 60 {
			samples = samples[len(samples)-60:]
		}
		for _, pt := range samples {
			valStr := ""
			if strings.EqualFold(metricType, "memory") {
				valStr = fmt.Sprintf("%.2f MB", pt.Value/(1024*1024))
			} else {
				valStr = fmt.Sprintf("%.4f Cores", pt.Value)
			}
			sb.WriteString(fmt.Sprintf("  %s -> %s\n", pt.Timestamp.Format(time.RFC3339), valStr))
		}
	}

	return tools.TextResult(sb.String(), meta)
}

func autodetectPrometheus(ctx context.Context, clientset *kubernetes.Clientset) (string, string) {
	namespaces := []string{"monitoring", "kube-system", "default"}
	selectors := []string{
		"app=prometheus",
		"app=prometheus-k8s",
		"app.kubernetes.io/name=prometheus",
		"prometheus=k8s",
	}

	for _, ns := range namespaces {
		for _, sel := range selectors {
			list, err := clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err == nil && len(list.Items) > 0 {
				return ns, list.Items[0].Name
			}
		}
	}

	// Fallback search
	list, err := clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, svc := range list.Items {
			if strings.Contains(strings.ToLower(svc.Name), "prometheus") {
				return svc.Namespace, svc.Name
			}
		}
	}

	return "monitoring", "prometheus-k8s" // Default fallback
}
