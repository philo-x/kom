package kubectl

import (
	"context"
	"encoding/json"
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
		mcp.WithDescription(
			"Read-only kubectl fallback tool. No shell pipes/redirects/external commands (grep,jq,awk).\n"+
				"Output format priority (lowest to highest token cost):\n"+
				"  1. -o jsonpath=<expr>  — extract only needed fields; use {\"\\t\"}/{\"\\n\"} for whitespace\n"+
				"     e.g. {.items[*].metadata.name} | {range .items[*]}{.metadata.name} {.status.readyReplicas}/{.status.replicas} {end}\n"+
				"  2. -l <label> / --field-selector — server-side filtering\n"+
				"  3. -o json — full object (managedFields & last-applied-configuration auto-stripped)\n"+
				"  4. default table — overview only",
		),
		mcp.WithTitleAnnotation("Execute Kubectl Command"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("cluster", mcp.Description("运行命令的集群（使用空字符串表示默认集群）/ Cluster where the command is executed (use empty string for default cluster)")),
		mcp.WithString("cmd", mcp.Description("要执行的 kubectl 命令字符串，例如 'get pods -n default'。不支持管道和外部命令。/ The kubectl command string to execute, e.g., 'get pods -n default'. Pipes and external commands are not supported.")),
		mcp.WithArray("args",
			mcp.Description("参数列表（可选，如果指定了 cmd，则优先使用 cmd 并解析） / The arguments list (optional)"),
			mcp.Items(map[string]interface{}{"type": "string"}),
		),
		mcp.WithNumber("page", mcp.Description("页码，仅在执行 get 命令列出资源时有效，从1开始（默认1）/ Page number, only valid for get commands, starting from 1 (default 1)")),
		mcp.WithNumber("pageSize", mcp.Description("每页行数或资源数，从1开始（默认100，最大500）/ Page size, starting from 1 (default 100, max 500)")),
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

	// 检测 Shell 运算符（管道、重定向等），直接执行时不经过 Shell，这些运算符无法被解释
	if op, found := detectShellOperator(args); found {
		return nil, fmt.Errorf(
			"不支持 Shell 运算符 %q。\n"+
				"kubectl MCP 工具通过 exec.Command 直接执行，不经过 Shell 解释器，管道（|）、重定向（>）及外部命令（grep、jq、awk）均无法使用。\n\n"+
				"建议替代方案：\n"+
				"  • 使用 -o jsonpath='...' 提取特定字段\n"+
				"  • 使用 --field-selector 或 -l 按标签/字段过滤\n"+
				"  • 使用 -o json 获取原始数据，由调用方在 Agent 逻辑中解析\n"+
				"/ Shell operator %q is not supported. kubectl MCP tool runs via exec.Command directly without a shell interpreter. "+
				"Pipes (|), redirects (>), and external commands (grep, jq, awk) cannot be used. "+
				"Use kubectl native options such as -o jsonpath, --field-selector, or -l for filtering.",
			op, op,
		)
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

	result := string(output)

	// 如果是 get 命令，应用分页处理
	if subcmd == "get" {
		pageVal := request.GetInt("page", 1)
		pageSizeVal := request.GetInt("pageSize", 100)
		if pageVal < 1 {
			pageVal = 1
		}
		if pageSizeVal < 1 {
			pageSizeVal = 100
		}
		if pageSizeVal > 500 {
			pageSizeVal = 500
		}

		if isJSONOutput(args) {
			result = processJSONOutput(result, pageVal, pageSizeVal)
		} else if !isCustomFormatting(args) {
			hasHeader := !hasNoHeadersFlag(args)
			result = paginateTable(result, pageVal, pageSizeVal, hasHeader)
		} else if isNameOutput(args) {
			result = paginateTable(result, pageVal, pageSizeVal, false)
		}
	} else if isJSONOutput(args) {
		// 非 get 命令但输出 json，只做噪音剥离
		result = stripJSONNoise(result)
	}

	return tools.TextResult(result, meta)
}

// isJSONOutput 检查参数列表中是否包含 -o json 或 --output json 或 --output=json
func isJSONOutput(args []string) bool {
	for i, arg := range args {
		switch {
		case arg == "-o" || arg == "--output":
			if i+1 < len(args) && args[i+1] == "json" {
				return true
			}
		case arg == "-o=json" || arg == "--output=json":
			return true
		}
	}
	return false
}

// stripJSONNoise 从 kubectl -o json 的输出中剥离对 LLM 无价值的高噪音字段：
//   - metadata.managedFields：Kubernetes 内部字段追踪信息
//   - metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]：apply 时记录的全量配置快照
func stripJSONNoise(raw string) string {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		// 解析失败则原样返回，不影响正常使用
		return raw
	}

	stripObject(obj)

	cleaned, err := json.MarshalIndent(obj, "", "    ")
	if err != nil {
		return raw
	}
	return string(cleaned)
}

// stripObject 递归处理 JSON 对象，清除噪音字段
func stripObject(obj map[string]interface{}) {
	// 处理 metadata 字段
	if meta, ok := obj["metadata"].(map[string]interface{}); ok {
		// 删除 managedFields
		delete(meta, "managedFields")

		// 删除 annotations 中的 last-applied-configuration
		if annotations, ok := meta["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
			// 如果 annotations 已空，也一并删除
			if len(annotations) == 0 {
				delete(meta, "annotations")
			}
		}
	}

	// 递归处理 items 列表（适用于 List 类型资源）
	if items, ok := obj["items"].([]interface{}); ok {
		for _, item := range items {
			if itemObj, ok := item.(map[string]interface{}); ok {
				stripObject(itemObj)
			}
		}
	}
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

// shellOperators 是需要检测的 Shell 运算符集合
var shellOperators = map[string]bool{
	"|": true, ">": true, "<": true, ">>": true, "<<": true,
	"&&": true, "||": true, ";": true, "&": true,
}

// detectShellOperator 检查参数列表中是否包含 Shell 运算符 token
// 若发现则返回该运算符字符串及 true，否则返回空字符串及 false
func detectShellOperator(args []string) (string, bool) {
	for _, arg := range args {
		if shellOperators[arg] {
			return arg, true
		}
	}
	return "", false
}

// parseCommandLine 解析命令行字符串为参数列表，遵循 POSIX Shell 引用规则：
// - 单引号内：所有字符（包括 \）逐字保留，直到下一个单引号
// - 双引号内：仅 \"、\\、\$、\` 有转义含义，其余 \ 原样保留
// - 引号外：\ 转义紧随其后的单个字符（含空格）
func parseCommandLine(cmd string) []string {
	var args []string
	var current strings.Builder
	inDoubleQuotes := false
	inSingleQuotes := false

	for i := 0; i < len(cmd); i++ {
		r := cmd[i]

		// 单引号模式：逐字保留所有内容，直到遇到配对的单引号
		if inSingleQuotes {
			if r == '\'' {
				inSingleQuotes = false
			} else {
				current.WriteByte(r)
			}
			continue
		}

		// 双引号模式：仅处理特定转义序列
		if inDoubleQuotes {
			if r == '\\' && i+1 < len(cmd) {
				next := cmd[i+1]
				// 双引号内仅以下字符可被 \ 转义
				if next == '"' || next == '\\' || next == '$' || next == '`' {
					current.WriteByte(next)
					i++
					continue
				}
			}
			if r == '"' {
				inDoubleQuotes = false
				continue
			}
			current.WriteByte(r)
			continue
		}

		// 引号外处理
		const singleQuote = byte('\'')
		switch r {
		case singleQuote:
			inSingleQuotes = true
		case '"':
			inDoubleQuotes = true
		case '\\':
			// 转义紧随其后的单个字符
			if i+1 < len(cmd) {
				current.WriteByte(cmd[i+1])
				i++
			}
		case ' ', '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// processJSONOutput 对 JSON 结果进行噪音剥离和分页切片处理
func processJSONOutput(raw string, page, pageSize int) string {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}

	// 1. 剥离高噪音字段
	stripObject(obj)

	// 2. 如果包含 items 数组，进行分页切片
	if items, ok := obj["items"].([]interface{}); ok {
		total := len(items)
		start := (page - 1) * pageSize
		end := start + pageSize
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		obj["items"] = items[start:end]

		// 注入分页元数据以便 Agent 感知
		obj["total"] = total
		obj["page"] = page
		obj["pageSize"] = pageSize
		totalPages := total / pageSize
		if total%pageSize > 0 {
			totalPages++
		}
		obj["totalPages"] = totalPages
	}

	cleaned, err := json.MarshalIndent(obj, "", "    ")
	if err != nil {
		return raw
	}
	return string(cleaned)
}

// paginateTable 对文本表格结果进行按行分页切片，保持表头
func paginateTable(raw string, page, pageSize int, hasHeader bool) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 {
		return raw
	}

	var header string
	var dataLines []string
	if hasHeader && len(lines) > 0 {
		header = lines[0]
		dataLines = lines[1:]
	} else {
		dataLines = lines
	}

	total := len(dataLines)
	if total == 0 {
		return raw
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	paginatedLines := dataLines[start:end]

	var resultLines []string
	if hasHeader {
		resultLines = append(resultLines, header)
	}
	resultLines = append(resultLines, paginatedLines...)

	totalPages := total / pageSize
	if total%pageSize > 0 {
		totalPages++
	}
	summary := fmt.Sprintf("\n[Pagination] Page: %d/%d, PageSize: %d, Total: %d", page, totalPages, pageSize, total)
	resultLines = append(resultLines, summary)

	return strings.Join(resultLines, "\n")
}

// isCustomFormatting 检查是否使用了 yaml, jsonpath, go-template 等自定义输出格式（需要排除在表格分页之外）
func isCustomFormatting(args []string) bool {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-o=") || strings.HasPrefix(arg, "--output=") {
			val := strings.SplitN(arg, "=", 2)[1]
			if val == "yaml" || strings.HasPrefix(val, "jsonpath") || strings.HasPrefix(val, "go-template") {
				return true
			}
		}
		if arg == "-o" || arg == "--output" {
			if i+1 < len(args) {
				val := args[i+1]
				if val == "yaml" || strings.HasPrefix(val, "jsonpath") || strings.HasPrefix(val, "go-template") {
					return true
				}
			}
		}
	}
	return false
}

// isNameOutput 检查是否使用了 -o name 格式
func isNameOutput(args []string) bool {
	for i, arg := range args {
		switch {
		case arg == "-o" || arg == "--output":
			if i+1 < len(args) && args[i+1] == "name" {
				return true
			}
		case arg == "-o=name" || arg == "--output=name":
			return true
		}
	}
	return false
}

// hasNoHeadersFlag 检查是否传入了 --no-headers 参数
func hasNoHeadersFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--no-headers" {
			return true
		}
	}
	return false
}
