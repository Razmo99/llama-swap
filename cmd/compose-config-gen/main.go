package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	labelPrefix             = "ai.llamaswap."
	labelEnabled            = labelPrefix + "enabled"
	labelModelID            = labelPrefix + "model_id"
	labelName               = labelPrefix + "name"
	labelDescription        = labelPrefix + "description"
	labelAliases            = labelPrefix + "aliases"
	labelProxy              = labelPrefix + "proxy"
	labelCheckEndpoint      = labelPrefix + "check_endpoint"
	labelTTL                = labelPrefix + "ttl"
	labelUseModelName       = labelPrefix + "use_model_name"
	labelConcurrencyLimit   = labelPrefix + "concurrency_limit"
	defaultCheckEndpoint    = "/health"
	defaultListenPort       = 8080
	defaultGeneratedComment = "# Generated from docker-compose.yml. Do not edit by hand.\n"
)

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Labels any `yaml:"labels"`
}

type generatedConfig struct {
	HealthCheckTimeout int                        `yaml:"healthCheckTimeout"`
	LogLevel           string                     `yaml:"logLevel"`
	Models             map[string]generatedModel `yaml:"models"`
}

type generatedModel struct {
	Name             string   `yaml:"name,omitempty"`
	Description      string   `yaml:"description,omitempty"`
	Cmd              string   `yaml:"cmd"`
	CmdStop          string   `yaml:"cmdStop"`
	Proxy            string   `yaml:"proxy"`
	CheckEndpoint    string   `yaml:"checkEndpoint,omitempty"`
	UnloadAfter      int      `yaml:"ttl,omitempty"`
	Aliases          []string `yaml:"aliases,omitempty"`
	UseModelName     string   `yaml:"useModelName,omitempty"`
	ConcurrencyLimit int      `yaml:"concurrencyLimit,omitempty"`
}

func main() {
	var composePath string
	var outputPath string
	var projectName string
	var envFile string
	var profile string
	var healthCheckTimeout int
	var logLevel string

	flag.StringVar(&composePath, "compose-file", "", "path to docker-compose.yml")
	flag.StringVar(&outputPath, "output", "", "path to generated llama-swap config")
	flag.StringVar(&projectName, "project-name", "project", "compose project name used for nested compose commands")
	flag.StringVar(&envFile, "env-file", "", "env file path used for nested compose commands")
	flag.StringVar(&profile, "profile", "models", "compose profile used for managed model services")
	flag.IntVar(&healthCheckTimeout, "health-check-timeout", 3600, "generated config healthCheckTimeout")
	flag.StringVar(&logLevel, "log-level", "info", "generated config logLevel")
	flag.Parse()

	if composePath == "" || outputPath == "" {
		exitf("compose-file and output are required")
	}

	cfg, err := loadCompose(composePath, envFile, profile)
	if err != nil {
		exitf("failed to load compose file: %v", err)
	}

	generated, err := buildGeneratedConfig(cfg, generationOptions{
		projectName:        projectName,
		envFile:            envFile,
		profile:            profile,
		healthCheckTimeout: healthCheckTimeout,
		logLevel:           logLevel,
		composePath:        composePath,
	})
	if err != nil {
		exitf("failed to build config: %v", err)
	}

	if err := writeConfig(outputPath, generated); err != nil {
		exitf("failed to write config: %v", err)
	}
}

type generationOptions struct {
	projectName        string
	envFile            string
	profile            string
	healthCheckTimeout int
	logLevel           string
	composePath        string
}

func loadCompose(path, envFile, profile string) (composeFile, error) {
	args := []string{"-f", path}
	if envFile = strings.TrimSpace(firstNonEmpty(envFile, os.Getenv("LLAMASWAP_COMPOSE_ENV_FILE"))); envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if profile = strings.TrimSpace(profile); profile != "" {
		args = append(args, "--profile", profile)
	}
	args = append(args, "config", "--format", "json")

	cmd := exec.Command("/usr/local/bin/docker-compose", args...)
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return composeFile{}, fmt.Errorf("docker-compose config failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return composeFile{}, err
	}

	var cfg composeFile
	if err := json.Unmarshal(output, &cfg); err != nil {
		return composeFile{}, err
	}

	return cfg, nil
}

func buildGeneratedConfig(composeCfg composeFile, opts generationOptions) (generatedConfig, error) {
	out := generatedConfig{
		HealthCheckTimeout: opts.healthCheckTimeout,
		LogLevel:           opts.logLevel,
		Models:             map[string]generatedModel{},
	}

	serviceNames := make([]string, 0, len(composeCfg.Services))
	for serviceName := range composeCfg.Services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		labels, err := normalizeLabels(composeCfg.Services[serviceName].Labels)
		if err != nil {
			return generatedConfig{}, fmt.Errorf("service %s labels: %w", serviceName, err)
		}

		if !parseBool(labels[labelEnabled]) {
			continue
		}

		modelID := strings.TrimSpace(labels[labelModelID])
		if modelID == "" {
			modelID = serviceName
		}

		model, err := buildModel(serviceName, modelID, labels, opts, composeCfg.Services)
		if err != nil {
			return generatedConfig{}, fmt.Errorf("service %s: %w", serviceName, err)
		}
		out.Models[modelID] = model
	}

	if len(out.Models) == 0 {
		return generatedConfig{}, errors.New("no model services were labeled with ai.llamaswap.enabled=true")
	}

	return out, nil
}

func buildModel(serviceName, modelID string, labels map[string]string, opts generationOptions, services map[string]composeService) (generatedModel, error) {
	cmd := []string{"/usr/local/bin/docker-compose"}
	if opts.projectName != "" {
		cmd = append(cmd, "-p", opts.projectName)
	}
	if opts.envFile != "" {
		cmd = append(cmd, "--env-file", opts.envFile)
	}
	if opts.profile != "" {
		cmd = append(cmd, "--profile", opts.profile)
	}
	cmd = append(cmd, "-f", opts.composePath)

	startTargets := []string{serviceName}
	stopTargets := []string{serviceName}
	if _, ok := services[serviceName+"-server"]; ok {
		startTargets = append(startTargets, serviceName+"-server")
		stopTargets = append(stopTargets, serviceName+"-server")
	}
	startCmd := append(append([]string{}, cmd...), "up")
	startCmd = append(startCmd, startTargets...)
	stopCmd := append(append([]string{}, cmd...), "stop")
	stopCmd = append(stopCmd, stopTargets...)

	proxy := strings.TrimSpace(labels[labelProxy])
	if proxy == "" {
		proxy = fmt.Sprintf("http://%s:%d", serviceName, defaultListenPort)
	}

	checkEndpoint := strings.TrimSpace(labels[labelCheckEndpoint])
	if checkEndpoint == "" {
		checkEndpoint = defaultCheckEndpoint
	}

	model := generatedModel{
		Name:          firstNonEmpty(strings.TrimSpace(labels[labelName]), serviceName),
		Description:   strings.TrimSpace(labels[labelDescription]),
		Cmd:           strings.Join(startCmd, " "),
		CmdStop:       strings.Join(stopCmd, " "),
		Proxy:         proxy,
		CheckEndpoint: checkEndpoint,
		Aliases:       parseCSV(labels[labelAliases]),
		UseModelName:  strings.TrimSpace(labels[labelUseModelName]),
	}

	if raw := strings.TrimSpace(labels[labelTTL]); raw != "" {
		ttl, err := strconv.Atoi(raw)
		if err != nil {
			return generatedModel{}, fmt.Errorf("invalid %s value %q", labelTTL, raw)
		}
		model.UnloadAfter = ttl
	}

	if raw := strings.TrimSpace(labels[labelConcurrencyLimit]); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return generatedModel{}, fmt.Errorf("invalid %s value %q", labelConcurrencyLimit, raw)
		}
		model.ConcurrencyLimit = limit
	}

	return model, nil
}

func normalizeLabels(value any) (map[string]string, error) {
	if value == nil {
		return map[string]string{}, nil
	}

	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]string, len(typed))
		for k, v := range typed {
			out[k] = fmt.Sprint(v)
		}
		return out, nil
	case []any:
		out := make(map[string]string, len(typed))
		for _, item := range typed {
			entry := fmt.Sprint(item)
			key, val, ok := strings.Cut(entry, "=")
			if !ok {
				return nil, fmt.Errorf("invalid label entry %q", entry)
			}
			out[strings.TrimSpace(key)] = strings.TrimSpace(val)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported labels type %T", value)
	}
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeConfig(path string, cfg generatedConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(defaultGeneratedComment), data...), 0o644)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
