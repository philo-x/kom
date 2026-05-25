package kubectl

import (
	"github.com/mark3labs/mcp-go/server"
)

func RegisterTools(s *server.MCPServer) {
	s.AddTool(
		KubectlTool(),
		KubectlHandler,
	)
}
