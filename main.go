package main

import (
	"flag"

	"github.com/weibaohui/kom/example"
	"github.com/weibaohui/kom/mcp"
	"k8s.io/klog/v2"
)

// main 初始化并启动带有认证信息注入的 MCP 服务端，支持通过 HTTP Header 注入用户名到请求上下文，实现权限控制。
func main() {
	klog.InitFlags(nil)
	flag.Set("v", "6")
	example.Connect()
	// 使用配置模式启动，显式指定为 SSE 模式
	cfg := &mcp.ServerConfig{
		Name:    "kom mcp server",
		Version: "0.0.1",
		Port:    9096,
		Mode:    mcp.ServerModeSSE, // 只开启 SSE，不开启 stdio 阻塞
	}
	mcp.RunMCPServerWithOption(cfg)

}
