---
name: Bug 报告
about: 报告一个可复现的问题（安全漏洞请勿走公开 Issue，见 SECURITY.md）
title: "[Bug] "
labels: bug
assignees: ""
---

## 问题描述

<!-- 发生了什么？期望发生什么？ -->

## 复现步骤

1.
2.
3.

## 环境

- 部署方式：docker compose / 单二进制 / harbor 镜像
- 版本或 commit：
- 浏览器（前端问题）：
- 涉及的渠道类型：api_key / OAuth（kimi / openai / anthropic / …）
- 协议入口（转发问题）：`/v1/chat/completions` / `/v1/messages` / `/v1/responses`

## 日志 / 错误信息

<!--
粘贴相关日志或响应即可，但必须先脱敏：
不要贴 API Key（sk-…）、供应商凭据、prompt 内容、邮箱等隐私信息。
-->

```text

```

## 补充信息

<!-- 配置差异（相关 .env 项）、截图、是否流式、是否可稳定复现等 -->

> ⚠️ 安全相关问题（隔离绕过、凭据泄露、鉴权绕过、SSRF 等）请不要提交公开 Issue，
> 走 [SECURITY.md](../SECURITY.md) 里的私密上报渠道。
