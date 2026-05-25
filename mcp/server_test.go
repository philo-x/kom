package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/callbacks"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools/dynamic"
	"github.com/weibaohui/kom/mcp/tools/event"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/homedir"
)

func initTestCluster(t *testing.T) {
	// Register the local default cluster if not already registered
	if len(kom.Clusters().AllClusters()) == 0 {
		kom.Clusters().SetRegisterCallbackFunc(callbacks.RegisterDenyMutationCallbacks)
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
		}
		_, err := kom.Clusters().RegisterByPathWithID(kubeconfig, "default")
		if err != nil {
			t.Skipf("Skipping integration test as K8s cluster registration failed: %v", err)
		}
	}
}

func TestMcpToolsRegistration(t *testing.T) {
	cfg := &ServerConfig{
		Name:    "test-server",
		Version: "1.0.0",
		Port:    9099,
		Mode:    ServerModeStdio,
	}
	s := GetMCPServerWithOption(cfg)
	if s == nil {
		t.Fatal("Failed to create MCPServer")
	}

	// Verify that deprecated tools are NOT registered
	list := s.ListTools()
	for _, tool := range list {
		if tool.Tool.Name == "list_k8s_pod_event" || tool.Tool.Name == "list_k8s_deploy_event" {
			t.Errorf("Deprecated tool %s should not be registered", tool.Tool.Name)
		}
	}

	// Verify that new tools ARE registered
	expectedTools := []string{
		"list_k8s_event",
		"diagnose_k8s_pod_network",
		"test_k8s_dns_resolve",
		"get_k8s_node_system_logs",
		"get_k8s_node_dmesg_oom",
		"diagnose_k8s_csi_driver",
		"get_k8s_resource_metrics_history",
	}

	for _, name := range expectedTools {
		found := false
		for _, tool := range list {
			if tool.Tool.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected tool %s to be registered, but it was not", name)
		}
	}
}

func TestSecretDataMasking(t *testing.T) {
	initTestCluster(t)

	// Create a temporary Secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcp-test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"password": []byte("super-secret-password-123"),
		},
		StringData: map[string]string{
			"username": "admin-user",
		},
	}

	kubectl := kom.Cluster("default")
	if kubectl == nil {
		t.Skip("Default cluster is not registered")
	}

	clientset := kubectl.Client()
	if clientset == nil {
		t.Fatal("Clientset is nil")
	}

	_ = clientset.CoreV1().Secrets("default").Delete(context.TODO(), "mcp-test-secret", metav1.DeleteOptions{})
	_, err := clientset.CoreV1().Secrets("default").Create(context.TODO(), secret, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test secret: %v", err)
	}
	defer clientset.CoreV1().Secrets("default").Delete(context.TODO(), "mcp-test-secret", metav1.DeleteOptions{})

	// Call dynamic get handler
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_k8s_resource",
			Arguments: map[string]interface{}{
				"cluster":   "default",
				"namespace": "default",
				"name":      "mcp-test-secret",
				"kind":      "Secret",
				"version":   "v1",
			},
		},
	}

	res, err := dynamic.GetDynamicResourceHandler(context.TODO(), req)
	if err != nil {
		t.Fatalf("GetDynamicResourceHandler failed: %v", err)
	}

	if len(res.Content) == 0 {
		t.Fatal("Response content is empty")
	}

	textContent, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("Expected TextContent, got %T", res.Content[0])
	}

	// Parse JSON
	var parsed map[string]interface{}
	err = json.Unmarshal([]byte(textContent.Text), &parsed)
	if err != nil {
		t.Fatalf("Failed to unmarshal response text: %v", err)
	}

	t.Logf("Response JSON: %s", textContent.Text)

	// Verify that data is masked
	data, ok := parsed["data"].(map[string]interface{})
	if !ok {
		t.Fatal("data field is missing or not a map")
	}

	if data["password"] != "*** [Base64 Obfuscated]" {
		t.Errorf("Expected masked password, got: %v", data["password"])
	}
}

func TestUnifiedEventsFilter(t *testing.T) {
	initTestCluster(t)

	// Call list events handler
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_k8s_event",
			Arguments: map[string]interface{}{
				"cluster":            "default",
				"namespace":          "default",
				"involvedObjectKind": "Pod",
			},
		},
	}

	_, err := event.ListEventResourceHandler(context.TODO(), req)
	if err != nil {
		t.Fatalf("ListEventResourceHandler failed: %v", err)
	}
}

func TestDescribeNamespaceWithoutEvents(t *testing.T) {
	initTestCluster(t)

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "describe_k8s_resource",
			Arguments: map[string]interface{}{
				"cluster":    "default",
				"name":       "default",
				"kind":       "Namespace",
				"version":    "v1",
				"showEvents": false,
			},
		},
	}

	res, err := dynamic.GetDynamicResourceDescribeHandler(context.TODO(), req)
	if err != nil {
		t.Fatalf("GetDynamicResourceDescribeHandler failed: %v", err)
	}

	if len(res.Content) == 0 {
		t.Fatal("Response content is empty")
	}

	textContent, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("Expected TextContent, got %T", res.Content[0])
	}

	t.Logf("Describe Namespace result: %s", textContent.Text)
}
