package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var providerContextKey = struct{ _ *string }{}

// Config 配置文件
type Config struct {
	Listen    string                 `json:"listen"`    // 监听地址
	Tokens    []string               `json:"tokens"`    // 鉴权令牌
	Providers map[string]*Provider   `json:"providers"` // [上游名称]上游 LLM 服务
	Models    map[string]*ModelEntry `json:"models"`    // [请求模型]上游模型
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

type RouterAi struct {
	cfg *Config
	mux http.Handler
}

func NewRouterAi(cfg Config) *RouterAi {
	return (&RouterAi{cfg: &cfg}).Build()
}

func (r *RouterAi) Build() *RouterAi {
	for name, provider := range r.cfg.Providers {
		provider.url, provider.err = url.Parse(provider.URL)
		if provider.err == nil && (provider.url.Scheme == "" || provider.url.Host == "") {
			provider.err = fmt.Errorf("invalid provider url, name=%s, url=%s", name, provider.URL)
		}
	}

	mux := NewRouter()
	mux.Router(func(router Router) {
		router.Use(WebAuth(r.cfg.Tokens))
		if len(r.cfg.Models) == 0 || len(r.cfg.Providers) == 0 {
			router.Handle("POST /v1/chat/completions", WebError(500, "no models configured"))
		} else {
			router.Handle("POST /v1/chat/completions", r.chatCompletions())
		}
		router.Handle("POST /responses/", r.responses())
		router.Handle("/models", r.models())
	})

	r.mux = mux
	return r
}

func (r *RouterAi) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

// chatCompletions 反向代理处理器, 通过请求的模型，转发请求到指定的上游模型
func (r *RouterAi) chatCompletions() http.Handler {
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
			WebWriteError(w, 400, err.Error())
			return
		}

		modelRequest := gjson.GetBytes(body, "model").String()
		if modelRequest == "" {
			WebWriteError(w, 400, "model not set")
			return
		}

		modelUpstream, ok := r.cfg.Models[modelRequest]
		if !ok {
			WebWriteError(w, 400, fmt.Sprintf("model not found: %s", modelRequest))
			return
		}

		provider, ok := r.cfg.Providers[modelUpstream.Provider]
		if !ok {
			WebWriteError(w, 500, fmt.Sprintf("provider not found: %s", modelRequest))
			return
		}

		if provider.err != nil {
			WebWriteError(w, 500, fmt.Sprintf("provider %s, err=%s", provider.Name, provider.err.Error()))
			return
		}

		if modelRequest != modelUpstream.Model {
			if body, err = sjson.SetBytes(body, "model", modelUpstream.Model); err != nil {
				WebWriteError(w, 400, err.Error())
				return
			}
		}

		// 设置请求体
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))

		providerProxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), providerContextKey, &provider)))
	})
}

func (r *RouterAi) responses() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
	})
}

// modelHandler 模型处理器, 返回所有可用的模型
func (r *RouterAi) models() http.Handler {
	models := make([]map[string]string, 0, len(r.cfg.Models))
	for model := range r.cfg.Models {
		models = append(models, map[string]string{"id": model, "object": "model", "owned_by": "proxy"})
	}
	slices.SortFunc(models, func(a, b map[string]string) int { return strings.Compare(a["id"], b["id"]) })

	data, err := json.Marshal(map[string]any{"object": "list", "data": models})
	if err != nil {
		return WebError(500, err.Error())
	}
	return WebBlob(200, "application/json", data)
}
