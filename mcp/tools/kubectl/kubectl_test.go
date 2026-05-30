package kubectl

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestParseCommandLine_SingleQuotePreservesBackslash 验证单引号内反斜杠原样保留（POSIX 语义）
func TestParseCommandLine_SingleQuotePreservesBackslash(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  `单引号内反斜杠保留（jsonpath \n 两字节）`,
			input: `get svc -o jsonpath='{"` + `\n` + `"}'`,
			want:  []string{"get", "svc", "-o", `jsonpath={"` + `\n` + `"}`},
		},
		{
			name:  `复杂 jsonpath 模板（含 \n 和 \t 两字节占位）`,
			input: `get svc --all-namespaces -o jsonpath='{range .items[*]}{.metadata.name}{"` + `\n` + `"}{end}'`,
			want: []string{
				"get", "svc", "--all-namespaces", "-o",
				`jsonpath={range .items[*]}{.metadata.name}{"` + `\n` + `"}{end}`,
			},
		},
		{
			name:  `单引号内的反斜杠不触发转义（hello\nworld）`,
			input: `echo 'hello` + `\n` + `world'`,
			want:  []string{"echo", `hello` + `\n` + `world`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommandLine(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCommandLine(%q)\n got:  %v\n want: %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseCommandLine_DoubleQuotes 验证双引号内的转义语义
func TestParseCommandLine_DoubleQuotes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "双引号内 \\\" 转义",
			input: `get svc -o json --output="name\"test"`,
			want:  []string{"get", "svc", "-o", "json", `--output=name"test`},
		},
		{
			name:  "双引号内 \\\\ 转义",
			input: `echo "path\\file"`,
			want:  []string{"echo", `path\file`},
		},
		{
			name:  "双引号内 \\n 不转义（保留原样）",
			input: `echo "line\nnext"`,
			want:  []string{"echo", `line\nnext`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommandLine(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCommandLine(%q)\n got:  %v\n want: %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseCommandLine_BasicSplit 验证基础空格分词
func TestParseCommandLine_BasicSplit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "基础多参数",
			input: "get pods -n default",
			want:  []string{"get", "pods", "-n", "default"},
		},
		{
			name:  "多余空格被忽略",
			input: "get  pods   -n   default",
			want:  []string{"get", "pods", "-n", "default"},
		},
		{
			name:  "空字符串",
			input: "",
			want:  nil,
		},
		{
			name:  "带引号的参数",
			input: `get pods -l "app=nginx"`,
			want:  []string{"get", "pods", "-l", "app=nginx"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommandLine(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCommandLine(%q)\n got:  %v\n want: %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseCommandLine_UnclosedQuoteError 验证未闭合引号报错
func TestParseCommandLine_UnclosedQuoteError(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"未闭合单引号", "get pods -n 'default"},
		{"未闭合双引号", `get pods -l "app=nginx`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCommandLine(tt.input)
			if err == nil {
				t.Errorf("expected error for unclosed quote in input %q, but got nil", tt.input)
			}
		})
	}
}

// TestDetectShellOperator_PipeBlocked 验证管道被正确检测
func TestDetectShellOperator_PipeBlocked(t *testing.T) {
	args1, err := parseCommandLine(`get svc --all-namespaces -o wide | grep -E "NodePort|30000"`)
	if err != nil {
		t.Fatalf("unexpected error parsing args1: %v", err)
	}
	op1, found1 := detectShellOperator(args1)
	if !found1 {
		t.Errorf("期望检测到管道 '|'，但未检测到。解析结果: %v", args1)
	} else if op1 != "|" {
		t.Errorf("期望运算符为 '|'，实际为 %q", op1)
	}

	args2, err := parseCommandLine(`get svc --all-namespaces -o json | jq -r '.items[]'`)
	if err != nil {
		t.Fatalf("unexpected error parsing args2: %v", err)
	}
	op2, found2 := detectShellOperator(args2)
	if !found2 {
		t.Errorf("期望检测到管道 '|'，但未检测到。解析结果: %v", args2)
	} else if op2 != "|" {
		t.Errorf("期望运算符为 '|'，实际为 %q", op2)
	}
}

// TestDetectShellOperator_AllOperators 验证所有 Shell 运算符均被检测
func TestDetectShellOperator_AllOperators(t *testing.T) {
	operators := []string{"|", ">", "<", ">>", "<<", "&&", "||", ";", "&"}
	for _, op := range operators {
		args := []string{"get", "pods", op, "something"}
		gotOp, found := detectShellOperator(args)
		if !found {
			t.Errorf("期望检测到 Shell 运算符 %q，但未检测到", op)
		}
		if gotOp != op {
			t.Errorf("期望运算符 %q，实际得到 %q", op, gotOp)
		}
	}
}

// TestDetectShellOperator_NormalCommand 验证正常命令不触发 Shell 运算符检测
func TestDetectShellOperator_NormalCommand(t *testing.T) {
	normalCmds := [][]string{
		{"get", "pods", "-n", "default"},
		{"get", "svc", "--all-namespaces", "-o", "jsonpath='{.items[*].metadata.name}'"},
		{"describe", "node", "node1"},
		{"logs", "pod/my-pod", "--tail=100"},
	}
	for _, args := range normalCmds {
		op, found := detectShellOperator(args)
		if found {
			t.Errorf("正常命令 %v 不应检测到 Shell 运算符，但检测到了 %q", args, op)
		}
	}
}

func TestPaginateTable(t *testing.T) {
	rawInput := `NAME      READY   STATUS    RESTARTS   AGE
pod-1     1/1     Running   0          10m
pod-2     1/1     Running   0          9m
pod-3     1/1     Running   0          8m
pod-4     1/1     Running   0          7m`

	// 1. With Header, Page=1, PageSize=2
	got := paginateTable(rawInput, 1, 2, true)
	want := `NAME      READY   STATUS    RESTARTS   AGE
pod-1     1/1     Running   0          10m
pod-2     1/1     Running   0          9m

[Pagination] Page: 1/2, PageSize: 2, Total: 4`
	if got != want {
		t.Errorf("paginateTable (with header, page 1) got:\n%s\nwant:\n%s", got, want)
	}

	// 2. With Header, Page=2, PageSize=2
	got = paginateTable(rawInput, 2, 2, true)
	want = `NAME      READY   STATUS    RESTARTS   AGE
pod-3     1/1     Running   0          8m
pod-4     1/1     Running   0          7m

[Pagination] Page: 2/2, PageSize: 2, Total: 4`
	if got != want {
		t.Errorf("paginateTable (with header, page 2) got:\n%s\nwant:\n%s", got, want)
	}

	// 3. Without Header (e.g. -o name or --no-headers)
	rawInputNoHeader := `pod/pod-1
pod/pod-2
pod/pod-3`
	got = paginateTable(rawInputNoHeader, 2, 2, false)
	want = `pod/pod-3

[Pagination] Page: 2/2, PageSize: 2, Total: 3`
	if got != want {
		t.Errorf("paginateTable (no header, page 2) got:\n%s\nwant:\n%s", got, want)
	}
}

func TestProcessJSONOutput(t *testing.T) {
	rawJSON := `{
		"apiVersion": "v1",
		"kind": "List",
		"items": [
			{"metadata": {"name": "p1", "managedFields": []}},
			{"metadata": {"name": "p2"}},
			{"metadata": {"name": "p3"}}
		]
	}`

	got := processJSONOutput(rawJSON, 1, 2)

	var res map[string]interface{}
	err := json.Unmarshal([]byte(got), &res)
	if err != nil {
		t.Fatalf("Failed to unmarshal paginated JSON: %v", err)
	}

	// Verify pagination metadata
	if res["total"] != float64(3) {
		t.Errorf("Expected total 3, got %v", res["total"])
	}
	if res["page"] != float64(1) {
		t.Errorf("Expected page 1, got %v", res["page"])
	}
	if res["pageSize"] != float64(2) {
		t.Errorf("Expected pageSize 2, got %v", res["pageSize"])
	}
	if res["totalPages"] != float64(2) {
		t.Errorf("Expected totalPages 2, got %v", res["totalPages"])
	}

	// Verify items length
	items, ok := res["items"].([]interface{})
	if !ok {
		t.Fatalf("Expected items to be an array, got %v", res["items"])
	}
	if len(items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items))
	}

	// Verify noise stripping (managedFields should be gone)
	item1, ok := items[0].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected item to be an object")
	}
	metadata, ok := item1["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected metadata to be an object")
	}
	if _, exists := metadata["managedFields"]; exists {
		t.Errorf("metadata.managedFields was not stripped!")
	}
}

func TestParseKubectlArgs(t *testing.T) {
	tests := []struct {
		args      []string
		wantSub   string
		wantMode  OutputMode
		wantWatch bool
	}{
		{[]string{"get", "pods"}, "get", OutputTable, false},
		{[]string{"get", "pods", "-o", "json"}, "get", OutputJSON, false},
		{[]string{"get", "pods", "-o=yaml"}, "get", OutputYAML, false},
		{[]string{"get", "pods", "--output=name"}, "get", OutputName, false},
		{[]string{"get", "pods", "-o", "jsonpath={.items[*].metadata.name}"}, "get", OutputJSONPath, false},
		{[]string{"logs", "pod/my-pod", "-f"}, "logs", OutputTable, true},
		{[]string{"get", "pods", "-w"}, "get", OutputTable, true},
		{[]string{"rollout", "status", "deployment/my-deploy"}, "rollout", OutputTable, false},
	}

	for _, tt := range tests {
		got := parseKubectlArgs(tt.args)
		if got.Subcommand != tt.wantSub {
			t.Errorf("parseKubectlArgs(%v).Subcommand = %q, want %q", tt.args, got.Subcommand, tt.wantSub)
		}
		if got.OutputMode != tt.wantMode {
			t.Errorf("parseKubectlArgs(%v).OutputMode = %q, want %q", tt.args, got.OutputMode, tt.wantMode)
		}
		if got.IsWatchLike != tt.wantWatch {
			t.Errorf("parseKubectlArgs(%v).IsWatchLike = %v, want %v", tt.args, got.IsWatchLike, tt.wantWatch)
		}
	}
}

func TestProjectJSON(t *testing.T) {
	rawJSON := `{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "my-pod",
			"namespace": "default",
			"uid": "12345"
		},
		"spec": {
			"priority": 100,
			"serviceGroup": {
				"services": ["s1", "s2"]
			}
		}
	}`

	fields := []string{".metadata.name", "spec.priority", ".spec.serviceGroup.services"}
	projected, err := projectJSON([]byte(rawJSON), fields)
	if err != nil {
		t.Fatalf("projectJSON failed: %v", err)
	}

	var res map[string]interface{}
	if err := json.Unmarshal(projected, &res); err != nil {
		t.Fatalf("failed to unmarshal projected json: %v", err)
	}

	// Verify projected fields exist
	meta, ok := res["metadata"].(map[string]interface{})
	if !ok || meta["name"] != "my-pod" {
		t.Errorf("metadata.name was not projected correctly, got: %v", res)
	}
	if _, ok := meta["namespace"]; ok {
		t.Errorf("metadata.namespace should have been filtered out")
	}

	spec, ok := res["spec"].(map[string]interface{})
	if !ok || spec["priority"] != float64(100) {
		t.Errorf("spec.priority was not projected correctly, got: %v", res)
	}

	sg, ok := spec["serviceGroup"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.serviceGroup was not projected, got: %v", res)
	}
	services, ok := sg["services"].([]interface{})
	if !ok || len(services) != 2 || services[0] != "s1" {
		t.Errorf("spec.serviceGroup.services was not projected correctly, got: %v", res)
	}
}
