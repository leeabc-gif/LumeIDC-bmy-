import { describe, expect, it } from 'vitest'
import { clientPluginComponent, pluginComponent } from './registry'

// 注册表是 PluginShell / ClientShell 取页面的唯一来源：
// 漏接线 = 后台菜单点进去「页面不存在」，前台入口 404，且构建期不会报错。
describe('插件页面注册表', () => {
  it('后台页已接线 refund', () => {
    expect(pluginComponent('refund')).toBeTypeOf('function')
  })

  it('后台页已接线 spaceship', () => {
    expect(pluginComponent('spaceship')).toBeTypeOf('function')
  })

  it('前台页已接线 refund', () => {
    expect(clientPluginComponent('refund')).toBeTypeOf('function')
  })

  it('前台页已接线 spaceship', () => {
    expect(clientPluginComponent('spaceship')).toBeTypeOf('function')
  })

  it('后台页已接线 violation', () => {
    expect(pluginComponent('violation')).toBeTypeOf('function')
  })

  it('前台页已接线 violation', () => {
    expect(clientPluginComponent('violation')).toBeTypeOf('function')
  })

  it('未注册插件返回 undefined（壳层据此提示未启用）', () => {
    expect(pluginComponent('not-exists')).toBeUndefined()
    expect(clientPluginComponent('not-exists')).toBeUndefined()
  })
})
