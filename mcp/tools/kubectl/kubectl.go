package kubectl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// KubectlTool 创建执行kubectl命令的工具
func KubectlTool() mcp.Tool {
	return mcp.NewTool(
		"kubectl",
		mcp.WithDescription("执行任意 kubectl 命令作为兜底工具 / Run any kubectl command as a fallback tool"),
		mcp.WithTitleAnnotation("Execute Kubectl Command"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("cluster", mcp.Description("运行命令的集群（使用空字符串表示默认集群）/ Cluster where the command is executed (use empty string for default cluster)")),
		mcp.WithString("cmd", mcp.Description("要执行的 kubectl 命令字符串，例如 'get pods -n default' / The kubectl command string to execute, e.g., 'get pods -n default'")),
		mcp.WithArray("args",
			mcp.Description("参数列表（可选，如果指定了 cmd，则优先使用 cmd 并解析） / The arguments list (optional)"),
			mcp.Items(map[string]interface{}{"type": "string"}),
		),
	)
}

var allowedSubcommands = map[string]bool{
	"get":           true,
	"describe":      true,
	"logs":          true,
	"explain":       true,
	"api-resources": true,
	"api-versions":  true,
	"version":       true,
	"cluster-info":  true,
	"top":           true,
	"options":       true,
}

// KubectlHandler 处理 kubectl 命令的执行请求
func KubectlHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	// 查找系统 PATH 中的 kubectl 可执行文件
	kubectlPath, err := exec.LookPath("kubectl")
	if err != nil {
		return nil, fmt.Errorf("kubectl command not found in PATH: %w", err)
	}

	// 解析或获取命令参数
	var args []string
	cmdStr := request.GetString("cmd", "")
	if cmdStr != "" {
		args = parseCommandLine(cmdStr)
	} else {
		args = request.GetStringSlice("args", []string{})
	}

	if len(args) == 0 {
		return nil, fmt.Errorf("either 'cmd' or 'args' must be provided and not empty")
	}

	// 如果第一个参数是 kubectl，则去除它
	if args[0] == "kubectl" {
		args = args[1:]
	}

	if len(args) == 0 {
		return nil, fmt.Errorf("command is empty after removing 'kubectl'")
	}

	// 检查仅允许只读操作的限制
	subcmd := strings.ToLower(args[0])
	if subcmd == "rollout" {
		if len(args) < 2 {
			return nil, fmt.Errorf("rollout command requires subcommands status or history")
		}
		nextSub := strings.ToLower(args[1])
		if nextSub != "status" && nextSub != "history" {
			return nil, fmt.Errorf("only read-only rollout commands (status, history) are allowed, '%s' is rejected", args[1])
		}
	} else if !allowedSubcommands[subcmd] {
		return nil, fmt.Errorf("only read-only commands (get, describe, logs, top, etc.) are allowed, command '%s' is rejected", args[0])
	}

	// 获取集群配置实例
	clusterInst := kom.Clusters().GetClusterById(meta.Cluster)
	if clusterInst == nil {
		return nil, fmt.Errorf("cluster %s not found", meta.Cluster)
	}

	// 动态生成临时 kubeconfig 文件以确保正确的连接配置
	kubeconfigPath, cleanup, err := writeKubeconfig(clusterInst.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to generate kubeconfig: %w", err)
	}
	defer cleanup()

	// 添加 --kubeconfig 参数来指定连接配置
	finalArgs := append([]string{"--kubeconfig", kubeconfigPath}, args...)

	// 执行命令
	cmd := exec.CommandContext(ctx, kubectlPath, finalArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl execution failed: %v, output: %s", err, string(output))
	}

	return tools.TextResult(string(output), meta)
}

// writeKubeconfig 将 rest.Config 转换为临时的 kubeconfig 文件
func writeKubeconfig(restConfig *rest.Config) (string, func(), error) {
	if restConfig == nil {
		return "", nil, fmt.Errorf("rest config is nil")
	}

	config := clientcmdapi.Config{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: map[string]*clientcmdapi.Cluster{
			"kom-cluster": {
				Server:                   restConfig.Host,
				CertificateAuthorityData: restConfig.TLSClientConfig.CAData,
				CertificateAuthority:     restConfig.TLSClientConfig.CAFile,
				InsecureSkipTLSVerify:    restConfig.TLSClientConfig.Insecure,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"kom-context": {
				Cluster:  "kom-cluster",
				AuthInfo: "kom-user",
			},
		},
		CurrentContext: "kom-context",
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kom-user": {
				ClientCertificateData: restConfig.TLSClientConfig.CertData,
				ClientCertificate:     restConfig.TLSClientConfig.CertFile,
				ClientKeyData:         restConfig.TLSClientConfig.KeyData,
				ClientKey:             restConfig.TLSClientConfig.KeyFile,
				Token:                 restConfig.BearerToken,
				TokenFile:             restConfig.BearerTokenFile,
				Username:              restConfig.Username,
				Password:              restConfig.Password,
			},
		},
	}

	if restConfig.ExecProvider != nil {
		config.AuthInfos["kom-user"].Exec = restConfig.ExecProvider
	}

	if restConfig.Impersonate.UserName != "" {
		config.AuthInfos["kom-user"].Impersonate = restConfig.Impersonate.UserName
		config.AuthInfos["kom-user"].ImpersonateGroups = restConfig.Impersonate.Groups
		config.AuthInfos["kom-user"].ImpersonateUserExtra = restConfig.Impersonate.Extra
	}

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "kubeconfig-kom-*.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp file for kubeconfig: %w", err)
	}
	tempFilePath := tempFile.Name()
	tempFile.Close()

	// 写入配置内容
	err = clientcmd.WriteToFile(config, tempFilePath)
	if err != nil {
		os.Remove(tempFilePath)
		return "", nil, fmt.Errorf("failed to write kubeconfig to temp file: %w", err)
	}

	cleanup := func() {
		os.Remove(tempFilePath)
	}

	return tempFilePath, cleanup, nil
}

// parseCommandLine 解析命令行字符串为参数列表，支持单/双引号及转义
func parseCommandLine(cmd string) []string {
	var args []string
	var current strings.Builder
	inDoubleQuotes := false
	inSingleQuotes := false
	escaped := false

	for i := 0; i < len(cmd); i++ {
		r := cmd[i]
		if escaped {
			current.WriteByte(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' && !inSingleQuotes {
			inDoubleQuotes = !inDoubleQuotes
			continue
		}
		if r == '\'' && !inDoubleQuotes {
			inSingleQuotes = !inSingleQuotes
			continue
		}
		if (r == ' ' || r == '\t') && !inDoubleQuotes && !inSingleQuotes {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteByte(r)
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}
