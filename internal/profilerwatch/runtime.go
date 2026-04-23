package profilerwatch

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"gopkg.in/yaml.v3"
)

// ResolvedRuntime contains the concrete endpoints and paths derived from the watcher config.
type ResolvedRuntime struct {
	CLIProxyConfigPath string `yaml:"cliproxy-config-path,omitempty"`
	PprofBaseURL       string `yaml:"pprof-base-url,omitempty"`
	PprofAddr          string `yaml:"pprof-addr,omitempty"`
	PprofPort          string `yaml:"pprof-port,omitempty"`
	PprofEnabled       bool   `yaml:"pprof-enabled"`
	RequestLogPath     string `yaml:"request-log-path,omitempty"`
	ServicePort        int    `yaml:"service-port,omitempty"`
	LoggingToFile      bool   `yaml:"logging-to-file"`
	AuthDir            string `yaml:"auth-dir,omitempty"`
}

type cliproxyRuntimeConfig struct {
	AuthDir       string `yaml:"auth-dir"`
	Port          int    `yaml:"port"`
	LoggingToFile bool   `yaml:"logging-to-file"`
	Pprof         struct {
		Enable bool   `yaml:"enable"`
		Addr   string `yaml:"addr"`
	} `yaml:"pprof"`
}

// ResolveRuntime derives concrete runtime locations from the watcher config and the target cliproxy config.
func ResolveRuntime(cfg *Config) (*ResolvedRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}

	resolved := &ResolvedRuntime{}
	var cliproxyCfg cliproxyRuntimeConfig

	if cfg.Target.CLIProxyConfigPath != "" {
		data, err := os.ReadFile(cfg.Target.CLIProxyConfigPath)
		if err != nil {
			return nil, fmt.Errorf("read target cliproxy config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cliproxyCfg); err != nil {
			return nil, fmt.Errorf("parse target cliproxy config: %w", err)
		}
		resolved.CLIProxyConfigPath = cfg.Target.CLIProxyConfigPath
		resolved.PprofEnabled = cliproxyCfg.Pprof.Enable
		resolved.PprofAddr = strings.TrimSpace(cliproxyCfg.Pprof.Addr)
		if resolved.PprofAddr == "" {
			resolved.PprofAddr = config.DefaultPprofAddr
		}
		resolved.ServicePort = cliproxyCfg.Port
		resolved.LoggingToFile = cliproxyCfg.LoggingToFile
		resolved.AuthDir = strings.TrimSpace(cliproxyCfg.AuthDir)

		if cfg.Target.RequestLogPath == "" && cliproxyCfg.LoggingToFile {
			authDir, err := util.ResolveAuthDir(cliproxyCfg.AuthDir)
			if err != nil {
				return nil, fmt.Errorf("resolve target auth-dir: %w", err)
			}
			if authDir != "" {
				resolved.RequestLogPath = filepath.Join(authDir, "logs", "main.log")
				resolved.AuthDir = authDir
			}
		}
	}

	if cfg.Target.RequestLogPath != "" {
		resolved.RequestLogPath = cfg.Target.RequestLogPath
	}

	pprofSource := cfg.Target.PprofURL
	if pprofSource == "" && resolved.PprofEnabled {
		pprofSource = resolved.PprofAddr
	}
	if pprofSource != "" {
		baseURL, err := normalizePprofBaseURL(pprofSource)
		if err != nil {
			return nil, fmt.Errorf("normalize pprof url: %w", err)
		}
		resolved.PprofBaseURL = baseURL
		resolved.PprofPort = extractURLPort(baseURL)
	}

	return resolved, nil
}

func normalizePprofBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" {
		parsed.Scheme = "http"
	}
	if parsed.Host == "" && parsed.Path != "" && !strings.HasPrefix(parsed.Path, "/") {
		parsed.Host = parsed.Path
		parsed.Path = ""
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("missing host in pprof url")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/debug/pprof") {
		if path == "" {
			path = "/debug/pprof"
		} else {
			path = path + "/debug/pprof"
		}
	}
	parsed.Path = path
	return strings.TrimRight(parsed.String(), "/"), nil
}

func extractURLPort(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := parsed.Host
	if host == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(host); err == nil {
		return port
	}
	return parsed.Port()
}
