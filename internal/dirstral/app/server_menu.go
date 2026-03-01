package app

const (
	serverActionStart  = "Start MCP Server"
	serverActionStatus = "MCP Server Status"
	serverActionRemote = "Remote MCP Status"
	serverActionLogs   = "View Logs"
	serverActionStop   = "Stop MCP Server"
	serverActionBack   = "Back"
)

func ServerMenuItems() []string {
	return []string{serverActionStart, serverActionStatus, serverActionRemote, serverActionLogs, serverActionStop, serverActionBack}
}
