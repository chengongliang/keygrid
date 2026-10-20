package upstream

import (
	"net/url"
	"os"
	"strings"
)

const (
	CNChatBaseDefault    = "https://copilot.tencent.com"
	DesktopVersionIntl   = "5.8.4"
	DesktopVersionCN     = "5.5.6"
	DesktopUAIntl        = "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"
	DesktopUACN          = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	CLIVersion           = "2.168.2"
	CLIUA                = "CLI/2.168.2 CodeBuddy/2.168.2"
	VSCodeVersion        = "1.119.0"
	VSCodeProductVersion = "4.12.39997873"
	VSCodeUA             = "VSCode/1.119.0 CodeBuddy/4.12.39997873"
)

func CNChatBase() string {
	raw := strings.TrimSpace(os.Getenv("WB_CN_CHAT_UPSTREAM"))
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Trim(parsed.Path, "/") != "" {
		return CNChatBaseDefault
	}
	return parsed.Scheme + "://" + parsed.Host
}

func CNEndpoint(target, overrideEnv string) string {
	if strings.TrimSpace(os.Getenv(overrideEnv)) != "" {
		return target
	}
	if strings.HasPrefix(target, CNChatBaseDefault+"/") {
		return CNChatBase() + strings.TrimPrefix(target, CNChatBaseDefault)
	}
	return target
}
