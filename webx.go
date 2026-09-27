package main

import (
	"cmp"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Router 路由器接口
type Router interface {
	Handle(string, http.Handler)
	Use(middlewares ...func(http.Handler) http.Handler)
	Chain(middlewares ...func(http.Handler) http.Handler) Router
	Router(useRouter func(router Router), middlewares ...func(http.Handler) http.Handler)
	Routes() func(func(string, string) bool)
	ServeHTTP(http.ResponseWriter, *http.Request)
}

type router struct {
	root        *router
	mux         *http.ServeMux
	middlewares []func(http.Handler) http.Handler
	routes      [][2]string
}

func NewRouter() Router {
	return &router{mux: http.NewServeMux()}
}

// Handle 处理 HTTP 请求
func (r *router) Handle(pattern string, handler http.Handler) {
	var route [2]string
	switch parts := strings.Fields(pattern); len(parts) {
	case 1:
		route = [2]string{"*", pattern}
	case 2:
		route = [2]string{parts[0], parts[1]}
	default:
		panic("invalid pattern: " + pattern)
	}

	if r.root != nil {
		r.root.routes = append(r.root.routes, route)
	} else {
		r.routes = append(r.routes, route)
	}

	for _, m := range slices.Backward(r.middlewares) {
		handler = m(handler)
	}
	r.mux.Handle(pattern, handler)
}

// Use 添加中间件
func (r *router) Use(middlewares ...func(http.Handler) http.Handler) {
	r.middlewares = append(r.middlewares, middlewares...)
}

// Chain 创建一个带有给定中间件的路由器
func (r *router) Chain(middlewares ...func(http.Handler) http.Handler) Router {
	return &router{root: r, mux: r.mux, middlewares: slices.Clone(r.middlewares)}
}

// Router 使用给定的路由器和中间件
func (r *router) Router(useRouter func(router Router), middlewares ...func(http.Handler) http.Handler) {
	useRouter(r.Chain(middlewares...))
}

// ServeHTTP 处理 HTTP 请求, 调用根路由器的 ServeHTTP 方法
func (r *router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.root != nil {
		r.root.mux.ServeHTTP(w, req)
	} else {
		r.mux.ServeHTTP(w, req)
	}
}

// Routes 获取所有路由，返回一个 {method, pattern} 的迭代器
func (r *router) Routes() func(func(string, string) bool) {
	return func(yield func(string, string) bool) {
		for _, route := range cmp.Or(r.root, r).routes {
			if !yield(route[0], route[1]) {
				break
			}
		}
	}
}

// Serve 启动 HTTP 服务器
func WebServe(ctx context.Context, listen string, mux http.Handler) <-chan error {
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

// WebAuth 认证中间件
func WebAuth(tokens []string) func(http.Handler) http.Handler {
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

// WebError 创建一个错误处理的 HTTP 处理程序
func WebError(code int, msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		WebWriteError(w, code, msg)
	}
}

// WebBlob 创建一个二进制数据处理的 HTTP 处理程序
func WebBlob(code int, contentType string, data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WebWriteBlob(w, code, contentType, data)
	}
}

// WebWriteError 写出统一错误响应
func WebWriteError(w http.ResponseWriter, code int, msg string) {
	if code == 0 {
		code = 500
	}
	WebWriteJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "proxy_error"}})
}

// WebWriteJSON 写出统一 JSON 响应
func WebWriteJSON(w http.ResponseWriter, code int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error":{"message":` + strconv.Quote(err.Error()) + `,"type":"proxy_error"}}`)
		code = 500
	}
	WebWriteBlob(w, code, "application/json", data)
}

// WebWriteBlob 写出二进制数据
func WebWriteBlob(w http.ResponseWriter, code int, contentType string, data []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if code > 0 {
		w.WriteHeader(code)
	}
	w.Write(data)
}
