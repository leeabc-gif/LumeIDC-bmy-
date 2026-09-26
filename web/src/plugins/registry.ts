import type { Component } from 'vue'

/**
 * 前端插件聚合注册表：插件名 → 页面组件（懒加载）。
 * 与后端 internal/plugins/all/all.go 对称：新增插件页面在此加一行映射。
 */

/** 后台插件页（PluginShell 使用，路由 /admin#/plugin/{name}） */
const adminRegistry: Record<string, () => Promise<Component>> = {
  webhooknotify: () => import('./webhooknotify/Admin.vue'),
  announcement: () => import('./announcement/Admin.vue'),
  tickets: () => import('./tickets/Admin.vue'),
  refund: () => import('./refund/Admin.vue'),
  spaceship: () => import('./spaceship/Admin.vue'),
  violation: () => import('./violation/Admin.vue'),
}

/** 前台用户中心插件页（ClientShell 使用，路由 /plugin/{name}） */
const clientRegistry: Record<string, () => Promise<Component>> = {
  refund: () => import('./refund/MyRefunds.vue'),
  spaceship: () => import('./spaceship/Client.vue'),
  violation: () => import('./violation/PublicList.vue'),
}

/** 按插件名取后台页组件加载器；未注册返回 undefined。 */
export function pluginComponent(name: string): (() => Promise<Component>) | undefined {
  return adminRegistry[name]
}

/** 按插件名取前台页组件加载器；未注册返回 undefined。 */
export function clientPluginComponent(name: string): (() => Promise<Component>) | undefined {
  return clientRegistry[name]
}
