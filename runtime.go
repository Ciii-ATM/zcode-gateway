package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var appVersion = envOr("ZCODE_APP_VERSION", "3.16.3")

func envSeconds(key string, fallback int) time.Duration {
	if raw := os.Getenv(key); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Duration(fallback) * time.Second
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func runtimeGOOS() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func runtimeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return runtime.GOARCH
	}
}

func osCategory() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func osVersion() string {
	if runtime.GOOS == "windows" {
		return "10.0.19045"
	}
	return "unknown"
}
func userAgentForProfile(profile string) string {
	switch profile {
	case "chrome":
		return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	case "claude-code":
		return "claude-cli/2.0.0 (external, cli)"
	default:
		return "ZCode/" + appVersion
	}
}

func titleForProfile(profile string) string {
	switch profile {
	case "chrome":
		return "Chrome"
	case "claude-code":
		return "Claude Code"
	default:
		return "Z Code@electron"
	}
}
