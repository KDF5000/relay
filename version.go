package relay

import "fmt"

// ProtocolVersion changes only when the Server/Node wire contract is no longer
// compatible. Version is injected into release binaries at build time.
const ProtocolVersion = "2"

var Version = "dev"

type BuildInfo struct {
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocol_version"`
}

func CurrentBuild() BuildInfo {
	return BuildInfo{Version: Version, ProtocolVersion: ProtocolVersion}
}

func VersionLine(component string) string {
	return fmt.Sprintf("%s %s (protocol %s)", component, Version, ProtocolVersion)
}
