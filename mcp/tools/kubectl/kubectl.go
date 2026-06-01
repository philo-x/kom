package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

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
				"Note: For complex queries (e.g. jsonpath, custom-columns, custom quoting), please use the 'args' parameter instead of 'cmd' to avoid parsing/quoting issues.\n"+
				"Output format priority (lowest to highest token cost):\n"+
				"  1. outputFields parameter — server-side JSON projection (recommended for JSON output to save tokens)\n"+
				"  2. -o jsonpath=<expr>  — extract only needed fields; use {\"\\t\"}/{\"\\n\"} for whitespace\n"+
				"  3. -l <label> / --field-selector — server-side filtering\n"+
				"  4. -o json — full object (managedFields & last-applied-configuration auto-stripped)\n"+
				"  5. default table — overview only",
		),
		mcp.WithTitleAnnotation("Execute Kubectl Command"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("运行命令的集群/Cluster where the command is executed")),
		mcp.WithString("cmd", mcp.Description("要执行的 kubectl 命令字符串，例如 'get pods -n default'。不支持管道和外部命令。/ The kubectl command string to execute, e.g., 'get pods -n default'. Pipes and external commands are not supported.")),
		mcp.WithArray("args",
			mcp.Description("参数列表（可选，如果指定了 cmd，则优先使用 cmd 并解析） / The arguments list (optional)"),
			mcp.Items(map[string]interface{}{"type": "string"}),
		),
		mcp.WithArray("outputFields",
			mcp.Description("JSON字段投影列表（可选，仅对 get -o json 有效）。例如：['.metadata.name', '.spec.priority'] / JSON fields projection list (optional, only valid for get -o json), e.g. ['.metadata.name', '.spec.priority']"),
			mcp.Items(map[string]interface{}{"type": "string"}),
		),
		mcp.WithNumber("page", mcp.Description("页码，仅在执行 get 命令列出资源时有效，从1开始（默认1）/ Page number, only valid for get commands, starting from 1 (default 1)")),
		mcp.WithNumber("pageSize", mcp.Description("每页行数或资源数，从1开始（默认100，最大300）/ Page size, starting from 1 (default 100, max 300)")),
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

	// 1. 设置 30s 默认超时，并遵守更短的 deadline
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 查找系统 PATH 中的 kubectl 可执行文件
	kubectlPath, err := exec.LookPath("kubectl")
	if err != nil {
		return nil, fmt.Errorf("kubectl command not found in PATH: %w", err)
	}

	// 解析或获取命令参数
	var args []string
	cmdStr := request.GetString("cmd", "")
	if cmdStr != "" {
		parsedArgs, err := parseCommandLine(cmdStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cmd: %w", err)
		}
		args = parsedArgs
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

	// 2. 初始化分类器
	parsedCmd := parseKubectlArgs(args)

	// 检查仅允许只读操作的限制
	subcmd := parsedCmd.Subcommand
	if subcmd == "rollout" {
		if len(parsedCmd.SubArgs) < 1 {
			return nil, fmt.Errorf("rollout command requires subcommands status or history")
		}
		nextSub := strings.ToLower(parsedCmd.SubArgs[0])
		if nextSub != "status" && nextSub != "history" {
			return nil, fmt.Errorf("only read-only rollout commands (status, history) are allowed, '%s' is rejected", nextSub)
		}
	} else if !allowedSubcommands[subcmd] {
		return nil, fmt.Errorf("only read-only commands (get, describe, logs, top, etc.) are allowed, command '%s' is rejected", args[0])
	}

	// 3. 拒绝长连接参数
	if parsedCmd.IsWatchLike {
		return nil, fmt.Errorf("streaming or watch commands (-w, --watch, -f, --follow) are not supported by the synchronous kubectl MCP tool")
	}

	// 4. 针对 rollout status，若未指定超时则自动补 --timeout=25s
	if subcmd == "rollout" && len(parsedCmd.SubArgs) > 0 && strings.ToLower(parsedCmd.SubArgs[0]) == "status" {
		hasTimeout := false
		for _, arg := range args {
			if strings.HasPrefix(arg, "--timeout") {
				hasTimeout = true
				break
			}
		}
		if !hasTimeout {
			args = append(args, "--timeout=25s")
		}
	}

	// 5. 支持 outputFields 字段投影
	outputFields := request.GetStringSlice("outputFields", []string{})
	if len(outputFields) > 0 && parsedCmd.OutputMode != OutputJSON {
		// 自动追加 -o json 输出格式
		args = append(args, "-o", "json")
		parsedCmd.OutputMode = OutputJSON
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

	// 6. 输出结果格式化与分页处理
	if subcmd == "get" {
		pageVal := request.GetInt("page", 1)
		pageSizeVal := request.GetInt("pageSize", 100)
		if pageVal < 1 {
			pageVal = 1
		}
		if pageSizeVal < 1 {
			pageSizeVal = 100
		}
		if pageSizeVal > 300 {
			pageSizeVal = 300
		}

		if parsedCmd.OutputMode == OutputJSON {
			// 剥离 JSON 噪音
			result = stripJSONNoise(result)
			// 服务端投影过滤
			if len(outputFields) > 0 {
				projected, err := projectJSON([]byte(result), outputFields)
				if err == nil {
					result = string(projected)
				}
			}
			// 分页切片
			result = processJSONOutput(result, pageVal, pageSizeVal)
		} else if parsedCmd.OutputMode == OutputTable {
			hasHeader := !hasNoHeadersFlag(args)
			result = paginateTable(result, pageVal, pageSizeVal, hasHeader)
		} else if parsedCmd.OutputMode == OutputName {
			result = paginateTable(result, pageVal, pageSizeVal, false)
		}
	} else if parsedCmd.OutputMode == OutputJSON {
		// 非 get 命令但输出 json，只做噪音剥离和可选投影
		result = stripJSONNoise(result)
		if len(outputFields) > 0 {
			projected, err := projectJSON([]byte(result), outputFields)
			if err == nil {
				result = string(projected)
			}
		}
	}

	return tools.TextResult(result, meta)
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
func parseCommandLine(cmd string) ([]string, error) {
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
	if inSingleQuotes || inDoubleQuotes {
		return nil, fmt.Errorf("unclosed quote in command line")
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args, nil
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

// hasNoHeadersFlag 检查是否传入了 --no-headers 参数
func hasNoHeadersFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--no-headers" {
			return true
		}
	}
	return false
}

// OutputMode 定义 kubectl 的输出格式模式
type OutputMode string

const (
	OutputTable         OutputMode = "table"
	OutputName          OutputMode = "name"
	OutputJSON          OutputMode = "json"
	OutputYAML          OutputMode = "yaml"
	OutputJSONPath      OutputMode = "jsonpath"
	OutputCustomColumns OutputMode = "custom-columns"
	OutputGoTemplate    OutputMode = "go-template"
)

// ParsedKubectl 结构化解析后的 kubectl 命令信息
type ParsedKubectl struct {
	Subcommand  string
	SubArgs     []string
	OutputMode  OutputMode
	IsWatchLike bool
}

// parseKubectlArgs 将参数列表解析为结构化的命令描述
func parseKubectlArgs(args []string) *ParsedKubectl {
	parsed := &ParsedKubectl{
		OutputMode: OutputTable,
	}

	if len(args) > 0 {
		parsed.Subcommand = strings.ToLower(args[0])
		parsed.SubArgs = args[1:]
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// 检查长连接模式（watch/follow）
		if arg == "-w" || arg == "--watch" || arg == "-f" || arg == "--follow" {
			parsed.IsWatchLike = true
		}

		// 识别输出格式
		if arg == "-o" || arg == "--output" {
			if i+1 < len(args) {
				parsed.OutputMode = parseOutputModeStr(args[i+1])
			}
		} else if strings.HasPrefix(arg, "-o=") {
			parsed.OutputMode = parseOutputModeStr(strings.TrimPrefix(arg, "-o="))
		} else if strings.HasPrefix(arg, "--output=") {
			parsed.OutputMode = parseOutputModeStr(strings.TrimPrefix(arg, "--output="))
		}
	}

	return parsed
}

func parseOutputModeStr(val string) OutputMode {
	// 去掉具体表达式的前缀，例如 jsonpath={...} -> jsonpath
	val = strings.SplitN(val, "=", 2)[0]
	switch val {
	case "json":
		return OutputJSON
	case "yaml":
		return OutputYAML
	case "name":
		return OutputName
	case "jsonpath", "jsonpath-as-json":
		return OutputJSONPath
	case "custom-columns", "custom-columns-file":
		return OutputCustomColumns
	case "go-template", "go-template-file":
		return OutputGoTemplate
	default:
		return OutputTable
	}
}

// projectJSON 对 JSON 数据进行字段投影提取
func projectJSON(raw []byte, fields []string) ([]byte, error) {
	var data interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}

	projected := projectValue(data, fields)
	return json.MarshalIndent(projected, "", "    ")
}

func projectValue(val interface{}, fields []string) interface{} {
	if len(fields) == 0 {
		return val
	}

	if m, ok := val.(map[string]interface{}); ok {
		// 如果是 Kubernetes 资源列表 (List 类型资源)
		if items, ok := m["items"].([]interface{}); ok {
			projectedItems := make([]interface{}, len(items))
			for i, item := range items {
				projectedItems[i] = projectSingleObject(item, fields)
			}

			// 组装并保留基本的 List 元信息
			res := make(map[string]interface{})
			for k, v := range m {
				if k != "items" {
					res[k] = v
				}
			}
			res["items"] = projectedItems
			return res
		}
		return projectSingleObject(m, fields)
	}
	return val
}

func projectSingleObject(obj interface{}, fields []string) interface{} {
	m, ok := obj.(map[string]interface{})
	if !ok {
		return obj
	}

	result := make(map[string]interface{})
	for _, field := range fields {
		// 移除前导点号
		path := strings.TrimPrefix(field, ".")
		parts := strings.Split(path, ".")

		val, found := getValueByPath(m, parts)
		if found {
			setNestedValue(result, parts, val)
		}
	}
	return result
}

func getValueByPath(obj map[string]interface{}, parts []string) (interface{}, bool) {
	var current interface{} = obj
	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		val, found := m[part]
		if !found {
			return nil, false
		}
		current = val
	}
	return current, true
}

func setNestedValue(obj map[string]interface{}, parts []string, val interface{}) {
	current := obj
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		next, exists := current[part]
		if !exists {
			nextMap := make(map[string]interface{})
			current[part] = nextMap
			current = nextMap
		} else if nextMap, ok := next.(map[string]interface{}); ok {
			current = nextMap
		} else {
			nextMap := make(map[string]interface{})
			current[part] = nextMap
			current = nextMap
		}
	}
	current[parts[len(parts)-1]] = val
}
