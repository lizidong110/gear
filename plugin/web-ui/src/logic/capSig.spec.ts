// 能力调用签名单测：防「双 ext.」回归（ctx.ext.ext.ai.*）。
import { describe, expect, it } from 'vitest'
import { capCallBase, capCallSig } from '@/logic/capSig'

describe('capCallBase（ctx.<name>.<method>）', () => {
  it('扩展能力名已含 ext. 命名空间，不再重复前缀', () => {
    expect(capCallBase('ext.ai', 'generate')).toBe('ctx.ext.ai.generate')
  })

  it('负向：绝不出现历史 bug 的双前缀 ctx.ext.ext.*', () => {
    expect(capCallBase('ext.ai', 'generate')).not.toContain('ctx.ext.ext.')
    for (const m of ['complete', 'react', 'find', 'generate', 'describe']) {
      expect(capCallBase('ext.ai', m)).toBe(`ctx.ext.ai.${m}`)
      expect(capCallBase('ext.ai', m)).not.toContain('ctx.ext.ext.')
    }
  })

  it('内置原语没有命名空间前缀', () => {
    expect(capCallBase('http', 'Request')).toBe('ctx.http.Request')
    expect(capCallBase('db', 'query')).toBe('ctx.db.query')
  })
})

describe('capCallSig（带实参样式的完整签名）', () => {
  it('ext 用单个具名 table', () => {
    expect(capCallSig({ name: 'ext.ai', kind: 'ext' }, 'generate')).toBe(
      'ctx.ext.ai.generate({...})',
    )
  })

  it('内置用位置参数', () => {
    expect(capCallSig({ name: 'http', kind: 'builtin' }, 'Request')).toBe('ctx.http.Request(...)')
  })

  it('withArgs=false 只给基名（复制签名用）', () => {
    expect(capCallSig({ name: 'ext.ai', kind: 'ext' }, 'generate', false)).toBe(
      'ctx.ext.ai.generate',
    )
  })
})
