package pod

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodDNSResolveTool() mcp.Tool {
	return mcp.NewTool(
		"test_k8s_dns_resolve",
		mcp.WithDescription("在Pod内部进行DNS解析测试，检查解析链条并诊断CoreDNS服务状态。 / Test DNS resolution inside Pod, check resolv.conf, and diagnose CoreDNS health."),
		mcp.WithTitleAnnotation("Test DNS Resolve"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("集群名称/Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("命名空间 / Namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("源 Pod 名称 / Source Pod name")),
		mcp.WithString("container", mcp.Description("容器名称 / Container name")),
		mcp.WithString("domain", mcp.Required(), mcp.Description("要解析的目标域名 (如 kubernetes.default.svc.cluster.local) / Target domain to resolve")),
	)
}

func TestPodDNSResolveHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	containerName := request.GetString("container", "")
	domain := request.GetString("domain", "")

	if domain == "" {
		return nil, fmt.Errorf("domain is required")
	}

	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx)

	// Step 1: Run nslookup inside the pod
	var resolveOutput string
	err = kubectl.Namespace(meta.Namespace).
		Name(meta.Name).
		Ctl().Pod().
		ContainerName(containerName).
		Command("sh", "-c", "nslookup "+domain).
		Execute(&resolveOutput).Error

	var sb strings.Builder
	if err == nil {
		sb.WriteString(fmt.Sprintf("DNS resolution successful inside pod:\n%s\n", resolveOutput))
		return tools.TextResult(sb.String(), meta)
	}

	sb.WriteString(fmt.Sprintf("nslookup failed with error: %v\nOutput: %s\n", err, resolveOutput))

	// Try dig if nslookup is not found
	if strings.Contains(err.Error(), "executable file not found") || strings.Contains(resolveOutput, "not found") {
		var digOutput string
		err = kubectl.Namespace(meta.Namespace).
			Name(meta.Name).
			Ctl().Pod().
			ContainerName(containerName).
			Command("sh", "-c", "dig "+domain).
			Execute(&digOutput).Error
		if err == nil {
			sb.WriteString(fmt.Sprintf("DNS resolution successful (via dig):\n%s\n", digOutput))
			return tools.TextResult(sb.String(), meta)
		}
	}

	// Step 2: Grab /etc/resolv.conf to inspect DNS configs
	var resolvConf string
	err = kubectl.Namespace(meta.Namespace).
		Name(meta.Name).
		Ctl().Pod().
		ContainerName(containerName).
		Command("cat", "/etc/resolv.conf").
		Execute(&resolvConf).Error
	if err == nil {
		sb.WriteString(fmt.Sprintf("\n--- Pod /etc/resolv.conf Configuration ---\n%s\n", resolvConf))
	} else {
		sb.WriteString(fmt.Sprintf("\nFailed to read /etc/resolv.conf: %v\n", err))
	}

	// Step 3: Check CoreDNS pod status in kube-system
	clientset := kubectl.Client()
	if clientset != nil {
		sb.WriteString("\n--- CoreDNS Pods Status (kube-system) ---\n")
		// Find pods with labels k8s-app=kube-dns or coredns
		podList, listErr := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
			LabelSelector: "k8s-app=kube-dns",
		})
		if listErr == nil && len(podList.Items) > 0 {
			for _, item := range podList.Items {
				sb.WriteString(fmt.Sprintf("Pod: %s, Status: %s, Ready: %v\n",
					item.Name, item.Status.Phase, isPodReady(&item)))
			}
		} else {
			// Fallback label selector
			podList, listErr = clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
				LabelSelector: "app=coredns",
			})
			if listErr == nil && len(podList.Items) > 0 {
				for _, item := range podList.Items {
					sb.WriteString(fmt.Sprintf("Pod: %s, Status: %s, Ready: %v\n",
						item.Name, item.Status.Phase, isPodReady(&item)))
				}
			} else {
				sb.WriteString("No coredns/kube-dns pods found in kube-system namespace.\n")
			}
		}
	}

	return tools.TextResult(sb.String(), meta)
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
