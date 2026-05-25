package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/weibaohui/kom/callbacks"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

const (
	defaultMCPName       = "kom mcp server"
	defaultMCPVersion    = "0.0.1"
	defaultMCPPort       = 9096
	defaultKubeconfigDir = "/etc/kom/kubeconfigs"
)

// main 初始化并启动带有认证信息注入的 MCP 服务端，支持通过 HTTP Header 注入用户名到请求上下文，实现权限控制。
func main() {
	klog.InitFlags(nil)
	flag.Set("v", "6")
	kom.Clusters().SetRegisterCallbackFunc(callbacks.RegisterDenyMutationCallbacks)

	kubeconfigs, err := loadKubeconfigs()
	if err != nil {
		klog.Fatalf("load kubeconfigs failed: %v", err)
	}

	if len(kubeconfigs) == 0 {
		if err := registerDefaultCluster(); err != nil {
			klog.Fatalf("register default cluster failed: %v", err)
		}
	}

	// 使用配置模式启动，显式指定为 SSE 模式
	cfg := &mcp.ServerConfig{
		Name:        envString("KOM_MCP_NAME", defaultMCPName),
		Version:     envString("KOM_MCP_VERSION", defaultMCPVersion),
		Port:        envInt("KOM_MCP_PORT", defaultMCPPort),
		Mode:        mcp.ServerModeSSE, // 只开启 SSE，不开启 stdio 阻塞
		Kubeconfigs: kubeconfigs,
	}
	mcp.RunMCPServerWithOption(cfg)

}

func loadKubeconfigs() ([]mcp.KubeconfigConfig, error) {
	kubeconfigDir := envString("KOM_KUBECONFIG_DIR", defaultKubeconfigDir)
	kubeconfigs, err := mcp.LoadKubeconfigsFromDirectory(kubeconfigDir)
	if err != nil {
		return nil, err
	}
	if len(kubeconfigs) > 0 {
		klog.Infof("Loaded %d kubeconfigs from %s", len(kubeconfigs), kubeconfigDir)
		return kubeconfigs, nil
	}

	klog.Warningf("No kubeconfigs found in %s, falling back to KUBECONFIG/default kubeconfig", kubeconfigDir)
	return nil, nil
}

func registerDefaultCluster() error {
	defaultKubeConfig := os.Getenv("KUBECONFIG")
	if defaultKubeConfig == "" {
		defaultKubeConfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
	}

	if _, err := os.Stat(defaultKubeConfig); err != nil {
		return fmt.Errorf("kubeconfig %s is not accessible: %w", defaultKubeConfig, err)
	}

	if _, err := kom.Clusters().RegisterByPathWithID(defaultKubeConfig, "dev"); err != nil {
		return err
	}
	kom.Clusters().Show()
	return nil
}

func envString(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func envInt(key string, fallback int) int {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	port, err := strconv.Atoi(val)
	if err != nil {
		klog.Warningf("Invalid %s=%q, using default %d", key, val, fallback)
		return fallback
	}
	return port
}
