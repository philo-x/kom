package pod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func DiagnosePodNetworkTool() mcp.Tool {
	return mcp.NewTool(
		"diagnose_k8s_pod_network",
		mcp.WithDescription("诊断Pod网络连通性。优先在容器内测试目标端口，若缺少工具则动态拉起临时诊断Pod进行测试。 / Diagnose pod network connectivity. First tries exec in the target pod, falls back to a temp diag pod if utilities are missing."),
		mcp.WithTitleAnnotation("Diagnose Pod Network"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("集群名称/Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("命名空间 / Namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("源 Pod 名称 / Source Pod name")),
		mcp.WithString("container", mcp.Description("容器名称 / Container name")),
		mcp.WithString("target", mcp.Required(), mcp.Description("目标 IP, 域名或 FQDN / Target IP, host or FQDN")),
		mcp.WithNumber("port", mcp.Required(), mcp.Description("目标端口 / Target port")),
		mcp.WithNumber("timeout", mcp.Description("超时时间（秒，默认 5 秒）/ Timeout in seconds (default 5)")),
	)
}

func DiagnosePodNetworkHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	containerName := request.GetString("container", "")
	target := request.GetString("target", "")
	port := request.GetInt("port", 0)
	timeout := request.GetInt("timeout", 5)
	if timeout <= 0 {
		timeout = 5
	}

	if target == "" || port <= 0 {
		return nil, fmt.Errorf("target and port are required and port must be positive")
	}

	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx)

	// Step 1: Try Exec nc / curl / wget inside the source Pod first
	execCommand := fmt.Sprintf("nc -z -w %d %s %d", timeout, target, port)
	var execResult string
	err = kubectl.Namespace(meta.Namespace).
		Name(meta.Name).
		Ctl().Pod().
		ContainerName(containerName).
		Command("sh", "-c", execCommand).
		Execute(&execResult).Error

	if err == nil {
		return tools.TextResult(fmt.Sprintf("Connection successful (verified via internal exec 'nc'):\n%s", execResult), meta)
	}

	// If it fails because nc is not found, try curl or wget
	if strings.Contains(err.Error(), "executable file not found") || strings.Contains(execResult, "not found") {
		execCommand = fmt.Sprintf("curl -I -s --connect-timeout %d http://%s:%d", timeout, target, port)
		err = kubectl.Namespace(meta.Namespace).
			Name(meta.Name).
			Ctl().Pod().
			ContainerName(containerName).
			Command("sh", "-c", execCommand).
			Execute(&execResult).Error
		if err == nil {
			return tools.TextResult(fmt.Sprintf("Connection successful (verified via internal exec 'curl'):\n%s", execResult), meta)
		}

		execCommand = fmt.Sprintf("wget -qO- --timeout=%d http://%s:%d", timeout, target, port)
		err = kubectl.Namespace(meta.Namespace).
			Name(meta.Name).
			Ctl().Pod().
			ContainerName(containerName).
			Command("sh", "-c", execCommand).
			Execute(&execResult).Error
		if err == nil {
			return tools.TextResult(fmt.Sprintf("Connection successful (verified via internal exec 'wget'):\n%s", execResult), meta)
		}
	}

	// If it was a network timeout error or refused inside the pod, return it directly
	if !strings.Contains(err.Error(), "executable file not found") && !strings.Contains(execResult, "not found") {
		return tools.TextResult(fmt.Sprintf("Connection failed inside pod (Command failed):\nError: %v\nOutput: %s", err, execResult), meta)
	}

	klog.V(2).Infof("Pod %s is missing networking utilities (nc/curl/wget). Spawning a temporary debug Pod...", meta.Name)

	// Step 2: Fallback - Spawn a temporary diagnostic pod in the same namespace
	clientset := kubectl.Client()
	if clientset == nil {
		return nil, fmt.Errorf("failed to get kubernetes clientset to spawn fallback pod")
	}

	diagPodName := fmt.Sprintf("network-diag-%d", time.Now().UnixNano()%100000)
	diagPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      diagPodName,
			Namespace: meta.Namespace,
			Labels: map[string]string{
				"app": "network-diag-mcp",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            "diag",
					Image:           "dev-apaas-harbor-app.mis.bcs/ai/busybox:1.36",
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"sh", "-c"},
					Args:            []string{fmt.Sprintf("echo 'Testing connection...' && nc -zv -w %d %s %d 2>&1", timeout, target, port)},
				},
			},
		},
	}

	_, createErr := clientset.CoreV1().Pods(meta.Namespace).Create(ctx, diagPod, metav1.CreateOptions{})
	if createErr != nil {
		return nil, fmt.Errorf("failed to create fallback diagnostic pod: %v", createErr)
	}

	// Cleanup the pod on function exit
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		clientset.CoreV1().Pods(meta.Namespace).Delete(cleanupCtx, diagPodName, metav1.DeleteOptions{})
	}()

	// Poll until completed
	var completed bool
	for i := 0; i < 20; i++ {
		time.Sleep(1 * time.Second)
		p, getErr := clientset.CoreV1().Pods(meta.Namespace).Get(ctx, diagPodName, metav1.GetOptions{})
		if getErr == nil {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				completed = true
				break
			}
		}
	}

	if !completed {
		return tools.TextResult("Timeout waiting for fallback diagnostic pod to complete connection test.", meta)
	}

	// Get logs
	req := clientset.CoreV1().Pods(meta.Namespace).GetLogs(diagPodName, &corev1.PodLogOptions{})
	podLogs, logErr := req.Stream(ctx)
	if logErr != nil {
		return nil, fmt.Errorf("failed to get logs from fallback diagnostic pod: %v", logErr)
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, copyErr := io.Copy(buf, podLogs)
	if copyErr != nil {
		return nil, fmt.Errorf("failed to read logs from fallback diagnostic pod: %v", copyErr)
	}

	output := buf.String()
	var status string
	if strings.Contains(output, "open") || strings.Contains(output, "succeeded") {
		status = "Connection successful (verified via fallback diagnostic pod)"
	} else {
		status = "Connection failed (verified via fallback diagnostic pod)"
	}

	return tools.TextResult(fmt.Sprintf("%s:\n%s", status, output), meta)
}
