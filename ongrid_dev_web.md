# ongrid web 前端开发指南

这是一个 **React 18 + TypeScript + Vite 5** 的单页应用，用于 ongrid AIOps 平台。

## 技术栈

| 类别 | 技术 |
|------|------|
| 构建 | Vite 5 |
| 框架 | React 18 + TypeScript 5 (strict) |
| 样式 | Tailwind CSS v3 (dark-only) |
| 路由 | React Router v6 |
| 状态管理 | Zustand (持久化到 localStorage) |
| HTTP | 原生 `fetch` |
| 图标 | lucide-react |
| 图表 | recharts |
| 终端 | xterm |
| Markdown | react-markdown + remark-gfm |

---

## 开发环境启动

```bash
cd D:\claude\ongrid\web
npm ci          # 安装依赖
npm run dev     # 启动开发服务器
```

开发服务器运行在 `http://localhost:5173`，API 请求 `/api/*` 会代理到 `http://localhost:8090`（后端 manager 服务）。

**注意**：前端开发需要后端服务运行在 `localhost:8090`，否则 API 调用会失败。

---

## 常用命令

| 命令 | 说明 |
|------|------|
| `npm run dev` | 启动开发服务器 (热更新) |
| `npm run build` | 生产构建 (输出到 `dist/`) |
| `npm run preview` | 预览生产构建 |
| `npm run lint` | ESLint 检查 |
| `npm run typecheck` | TypeScript 类型检查 |
| `npm run test` | 运行单元测试 |
| `npm run test:watch` | 监听模式运行测试 |
| `npm run test:e2e` | Playwright E2E 测试 |

---

## 目录结构

```
web/
├── src/           # 源代码
├── public/        # 静态资源
├── e2e/           # E2E 测试
├── index.html     # 入口 HTML
├── vite.config.ts # Vite 配置 (含 API 代理)
├── tailwind.config.ts
├── tsconfig.json
└── vitest.config.ts
```

---

## API 代理配置

开发环境下，`vite.config.ts` 配置了 API 代理：

```typescript
server: {
  port: 5173,
  proxy: {
    '/api': {
      target: 'http://localhost:8090',  // 后端 manager 地址
      changeOrigin: true,
    },
  },
}
```

---

## 调试建议

1. **前端独立调试**：直接运行 `npm run dev`
2. **联调后端**：确保后端 `ongrid` 服务运行在 `8090` 端口
3. **检查认证**：登录后 token 存储在 `localStorage.ongrid.auth`
4. **401 处理**：客户端自动清除 session 并跳转 `/login`
