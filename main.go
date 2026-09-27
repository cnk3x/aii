package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/tidwall/jsonc"
	"sigs.k8s.io/yaml"
)

func main() {
	pflag.CommandLine.SortFlags = false
	pflag.StringP("config", "c", "", "")
	pflag.BoolP("debug", "d", false, "")
	pflag.Parse()

	if debug, _ := pflag.CommandLine.GetBool("debug"); debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGABRT, syscall.SIGTERM)
	defer stop()

	configFile, _ := pflag.CommandLine.GetString("config")
	configFile = cmp.Or(configFile, filepath.Join("data", "config.yaml"))

	var config Config
	if err := FileUnmarshal(configFile, &config); err != nil {
		slog.Error("load config", "path", configFile, "err", err)
		os.Exit(1)
	}

	slog.Info("listening", "listen", config.Listen, "providers", len(config.Providers))
	if err := <-WebServe(ctx, config.Listen, NewRouterAi(config)); err != nil {
		slog.Error("done", "err", err)
		os.Exit(1)
	}
	slog.Info("done")
}

// FileUnmarshal 从 file 读取文本并以JSON/YAML格式解码。支持JSON Comment。
func FileUnmarshal[T any](file string, cfg *T) (err error) {
	raw, e := os.ReadFile(file)
	if err = e; err != nil {
		if os.IsNotExist(err) {
			err = nil
		} else {
			err = fmt.Errorf("readFile %s: %w", file, err)
		}
		return
	}

	if len(raw) == 0 {
		return
	}

	switch ext := strings.ToLower(filepath.Ext(file)); ext {
	case ".json", ".jsonc":
		raw = jsonc.ToJSON(raw)
	case ".yaml", ".yml":
		raw, err = yaml.YAMLToJSON(raw)
	default:
		return
	}

	if len(raw) == 0 || err != nil {
		return
	}

	if err = json.Unmarshal(raw, cfg); err != nil {
		err = fmt.Errorf("json unmarshal: %w", err)
		return
	}

	return
}
