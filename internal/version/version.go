package version

import "InfraPulseAgent/internal/model"

var Version = "dev"
var Commit = "unknown"
var BuildTime = "unknown"

func Info() model.BuildInfo {
	return model.BuildInfo{Version: Version, Commit: Commit, BuildTime: BuildTime}
}
