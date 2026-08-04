# OpenWiki Instructions

## Scope
- Focus areas: manager（云端控制平面）、edge agent（边端数据采集）、aiops（AI Agent 对话）、iam（认证授权）、frontend（React SPA）
- Skip / deprioritize: 第三方 vendor 代码、生成代码（api/gen）、node_modules

## Terminology
- Prefer: OnGrid（项目名）、manager（云端）、edge（边端）、aiops（AI 运维）、RCA（根因分析）
- Avoid: 不要把 manager 称为 "backend"，它是控制平面；edge 是数据平面

## Audience
- Primary: coding agents (Cursor, Claude Code, Codex, Trae)
- Secondary: human onboarding

## Update preferences
- Prefer incremental updates from git diffs
- Keep pages under ~200 lines when possible
