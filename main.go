package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/tidwall/sjson"
	"sigs.k8s.io/yaml"
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "%s -c/--config <config file path>\n", filepath.Base(os.Args[0]))
	}
}

func main() {
	var configFile string

	flag.StringVar(&configFile, "c", "", "")
	flag.StringVar(&configFile, "config", "", "")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGABRT, syscall.SIGTERM)
	defer stop()

	configFile = cmp.Or(configFile, filepath.Join("data", "config.yaml"))

	var config Config
	if err := unmarshalFile(configFile, &config); err != nil {
		slog.Error("load config", "path", configFile, "err", err)
		os.Exit(1)
	}

	slog.Info("listening", "listen", config.Listen, "providers", len(config.Providers))
	if err := <-webServe(ctx, config.Listen, routerBuild(&config)); err != nil {
		slog.Error("done", "err", err)
		os.Exit(1)
	}
	slog.Info("done")
}

// Config 配置文件
type Config struct {
	Listen string   `json:"listen"` // 监听地址
	Tokens []string `json:"tokens"` // 鉴权令牌

	Providers map[string]Provider   `json:"providers"` // [上游名称]上游 LLM 服务
	Models    map[string]ModelEntry `json:"models"`    // [请求模型]上游模型
}

// Provider 描述一个上游 LLM 服务。
type Provider struct {
	Name   string `json:"name"`   // 服务名称
	URL    string `json:"url"`    // 服务地址
	Header string `json:"header"` // 请求头
	Token  string `json:"token"`  // 鉴权令牌

	err error
	url *url.URL
}

// ModelEntry 模型映射， name -> provider/model
type ModelEntry struct {
	Provider string `json:"provider"` // 转发到上游
	Model    string `json:"model"`    // 转发模型名称
}

func routerBuild(cfg *Config) http.Handler {
	for name := range cfg.Providers {
		provider := cfg.Providers[name]
		provider.url, provider.err = url.Parse(provider.URL)
		if provider.err == nil && (provider.url.Scheme == "" || provider.url.Host == "") {
			provider.err = fmt.Errorf("invalid provider url, name=%s, url=%s", name, provider.URL)
		}
		cfg.Providers[name] = provider
	}

	auth := tokenAuth(cfg.Tokens)

	mux := http.NewServeMux()
	mux.Handle("POST /chat/completions", auth(chatCompletionsHandler(cfg)))
	mux.Handle("/models", auth(modelHandler(cfg)))
	return mux
}

// chatCompletionsHandler 反向代理处理器, 通过请求的模型，转发请求到指定的上游模型
func chatCompletionsHandler(cfg *Config) http.Handler {
	if len(cfg.Models) == 0 || len(cfg.Providers) == 0 {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			apiError(w, 500, "no models configured")
		})
	}

	providerContextKey := struct{ _ *string }{}
	providerProxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			provider, ok := pr.In.Context().Value(providerContextKey).(*Provider)
			if !ok || provider.url == nil {
				return
			}

			// 目标地址
			pr.Out.Host = provider.url.Host
			pr.Out.URL.Scheme, pr.Out.URL.Host = provider.url.Scheme, provider.url.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = provider.url.Path, provider.url.RawPath
			// 合并查询参数，上游参数覆盖客户端
			if q := provider.url.Query(); len(q) > 0 {
				merged := pr.Out.URL.Query()
				maps.Copy(merged, q)
				pr.Out.URL.RawQuery = merged.Encode()
			}

			// 鉴权替换
			if provider.Header != "" {
				pr.Out.Header.Del(provider.Header)
			}
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")

			if provider.Token != "" {
				header := cmp.Or(provider.Header, "Authorization")
				if strings.EqualFold(header, "Authorization") {
					pr.Out.Header.Set("Authorization", "Bearer "+provider.Token)
				} else {
					pr.Out.Header.Set(header, provider.Token)
				}
			}
		},
	}

	const maxBodyBytes = 32 << 20 // 32 MiB
	readBody := func(body io.ReadCloser) ([]byte, error) {
		defer body.Close()
		return io.ReadAll(io.LimitReader(body, maxBodyBytes))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := readBody(req.Body)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}

		modelRequest := gjson.GetBytes(body, "model").String()
		if modelRequest == "" {
			apiError(w, 400, "model not set")
			return
		}

		modelUpstream, ok := cfg.Models[modelRequest]
		if !ok {
			apiError(w, 400, fmt.Sprintf("model not found: %s", modelRequest))
			return
		}

		provider, ok := cfg.Providers[modelUpstream.Provider]
		if !ok {
			apiError(w, 500, fmt.Sprintf("provider not found: %s", modelRequest))
			return
		}

		if provider.err != nil {
			apiError(w, 500, fmt.Sprintf("provider %s, err=%s", provider.Name, provider.err.Error()))
			return
		}

		if body, err = sjson.SetBytes(body, "model", modelUpstream.Model); err != nil {
			apiError(w, 400, err.Error())
			return
		}

		// 设置请求体
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))

		providerProxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), providerContextKey, &provider)))
	})
}

// modelHandler 模型处理器, 返回所有可用的模型
func modelHandler(cfg *Config) http.Handler {
	models := make([]map[string]string, 0, len(cfg.Models))
	for model := range cfg.Models {
		models = append(models, map[string]string{"id": model, "object": "model", "owned_by": "proxy"})
	}
	slices.SortFunc(models, func(a, b map[string]string) int { return strings.Compare(a["id"], b["id"]) })

	data, err := json.Marshal(map[string]any{"object": "list", "data": models})
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiError(w, 500, err.Error())
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webBlob(w, 200, "application/json", data)
	})
}

// unmarshalFile 从 file 读取文本并以JSON/YAML格式解码。支持JSON Comment。
func unmarshalFile[T any](file string, cfg *T) (err error) {
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

// Serve 启动 HTTP 服务器
func webServe(ctx context.Context, listen string, mux http.Handler) <-chan error {
	s := &http.Server{
		Addr:              listen,
		Handler:           mux,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	done := make(chan struct{})
	serveErr := make(chan error, 1)

	go func() {
		defer close(done)
		defer close(serveErr)
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
			s.Shutdown(sctx)
			cancel()
		}
	}()

	return serveErr
}

// apiError 写出统一错误响应
func apiError(w http.ResponseWriter, code int, msg string) {
	if code == 0 {
		code = 500
	}
	apiJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "proxy_error"}})
}

// apiJSON 写出统一 JSON 响应
func apiJSON(w http.ResponseWriter, code int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error":{"message":` + strconv.Quote(err.Error()) + `,"type":"proxy_error"}}`)
		code = 500
	}
	webBlob(w, code, "application/json", data)
}

func webBlob(w http.ResponseWriter, code int, contentType string, data []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if code > 0 {
		w.WriteHeader(code)
	}
	w.Write(data)
}

func tokenAuth(tokens []string) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		if len(tokens) == 0 {
			return handler
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get("Authorization")
			if tok == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			bearer := "bearer "
			if len(tok) < len(bearer) || !strings.EqualFold(tok[:len(bearer)], bearer) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			if !slices.Contains(tokens, tok[len(bearer):]) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			handler.ServeHTTP(w, r)
		})
	}
}
