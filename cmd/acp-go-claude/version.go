package main

import (
	"runtime/debug"
	"strings"
)

const (
	developmentVersion      = "dev"
	buildInfoVCSModifiedKey = "vcs.modified"
	buildInfoVCSRevisionKey = "vcs.revision"
)

var readBuildInfo = debug.ReadBuildInfo

func buildVersion() string {
	info, ok := readBuildInfo()
	if !ok {
		return developmentVersion
	}

	if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
		return version
	}

	revision, modified := "", false

	for _, setting := range info.Settings {
		switch setting.Key {
		case buildInfoVCSRevisionKey:
			revision = strings.TrimSpace(setting.Value)
		case buildInfoVCSModifiedKey:
			modified = setting.Value == "true"
		}
	}

	if revision == "" {
		return developmentVersion
	}

	if len(revision) > 12 {
		revision = revision[:12]
	}

	if modified {
		return revision + "-dirty"
	}

	return revision
}
