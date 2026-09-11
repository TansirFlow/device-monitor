package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// 看板前端直接编译进二进制：部署只有一个文件，不需要 nginx 托管静态资源，
// 也不会因为漏拷 web 目录而 404。
//
//go:embed web
var webFiles embed.FS

func webHandler() http.Handler {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源带内容指纹意义不大（单机部署），这里只禁用协商缓存，
		// 避免刷新看板拿到旧版 JS 而出现"改了没生效"的错觉。
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
