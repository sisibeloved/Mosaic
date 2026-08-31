import { defineConfig } from "vite";

// 开发态：vite dev server 代理 API 到本机 mosaic-server（契约同源）；
// 构建产物 dist/ 入库并由 mosaic-server 经 go:embed 服务（apps/web/embed.go）。
export default defineConfig({
  build: {
    outDir: "dist",
    target: "es2022",
  },
  server: {
    proxy: {
      "/v1": "http://127.0.0.1:7420",
    },
  },
});
