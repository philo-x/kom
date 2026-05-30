package alb2

import (
	"github.com/mark3labs/mcp-go/server"
)

// RegisterTools registers ALB2-related diagnostic and analysis tools to the MCP server
func RegisterTools(s *server.MCPServer) {
	s.AddTool(
		ListALB2ResourcesTool(),
		ListALB2ResourcesHandler,
	)
	s.AddTool(
		ListALB2RoutingRulesTool(),
		ListALB2RoutingRulesHandler,
	)
	s.AddTool(
		DiagnoseALB2RuleConflictTool(),
		DiagnoseALB2RuleConflictHandler,
	)
	s.AddTool(
		GetALB2ControllerLogsTool(),
		GetALB2ControllerLogsHandler,
	)
	s.AddTool(
		FindALB2ResourcesByServiceTool(),
		FindALB2ResourcesByServiceHandler,
	)
}
