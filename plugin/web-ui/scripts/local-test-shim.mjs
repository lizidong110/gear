// 极简 vitest 兼容层：只为在「依赖装不上」的环境里直接跑真实断言用例。
//
// 为什么值得单独写一个：单测断言的是**与内核对齐的语义**（依赖推断、模板预检、
// 错误码映射），这些恰恰是最不能靠肉眼确认的部分。构建工具链装不上时，
// 不能因此把「没人验证过」的代码交出去 —— 那正是本项目最忌讳的静默降级。
//
// 支持范围（够用即止，不做通用测试框架）：describe / it / expect + 本项目用到的匹配器。

let passed = 0
let failed = 0
const failures = []
let suite = ''

export function describe(name, fn) {
  const prev = suite
  suite = prev ? `${prev} › ${name}` : name
  fn()
  suite = prev
}

export function it(name, fn) {
  const full = suite ? `${suite} › ${name}` : name
  try {
    fn()
    passed += 1
  } catch (e) {
    failed += 1
    failures.push({ name: full, message: e?.message ?? String(e) })
  }
}

function deepEqual(a, b) {
  return JSON.stringify(a) === JSON.stringify(b)
}

function show(v) {
  try {
    return JSON.stringify(v)
  } catch {
    return String(v)
  }
}

class Assertion {
  constructor(actual, negated = false) {
    this.actual = actual
    this.negated = negated
  }

  get not() {
    return new Assertion(this.actual, !this.negated)
  }

  #check(ok, msg) {
    if (ok === this.negated) throw new Error(msg)
  }

  toBe(expected) {
    this.#check(Object.is(this.actual, expected), `期望 ${show(expected)}，实际 ${show(this.actual)}`)
  }

  // vitest 的原生断言；此前 shim 漏了它，导致「枚举与内核一致」这条防漂移断言在
  // gate:local 里报 `toBeTruthy is not a function`，而 vitest 下却是绿的——
  // 两条闸门口径不一致会让「本地全绿」变成假象，故补齐并保留 #check 的 not 语义。
  toBeTruthy() {
    this.#check(Boolean(this.actual), `期望为真值，实际 ${show(this.actual)}`)
  }

  toEqual(expected) {
    this.#check(deepEqual(this.actual, expected), `期望 ${show(expected)}，实际 ${show(this.actual)}`)
  }

  toContain(expected) {
    const ok = typeof this.actual === 'string' ? this.actual.includes(expected) : (this.actual ?? []).includes(expected)
    this.#check(ok, `期望包含 ${show(expected)}，实际 ${show(this.actual)}`)
  }

  toHaveLength(n) {
    this.#check(this.actual?.length === n, `期望长度 ${n}，实际 ${this.actual?.length}（${show(this.actual)}）`)
  }

  toMatch(re) {
    const r = re instanceof RegExp ? re : new RegExp(re)
    this.#check(r.test(String(this.actual)), `期望匹配 ${r}，实际 ${show(this.actual)}`)
  }

  toThrowError(re) {
    let msg = null
    try {
      this.actual()
    } catch (e) {
      msg = e?.message ?? String(e)
    }
    if (msg === null) throw new Error('期望抛出错误，但没有抛')
    if (re !== undefined) {
      const r = re instanceof RegExp ? re : new RegExp(re)
      this.#check(r.test(msg), `错误信息不匹配 ${r}，实际 ${show(msg)}`)
    } else {
      this.#check(true, '')
    }
  }

  toBeNull() {
    this.#check(this.actual === null, `期望 null，实际 ${show(this.actual)}`)
  }

  toBeInstanceOf(cls) {
    this.#check(this.actual instanceof cls, `期望是 ${cls?.name} 的实例`)
  }

  toBeLessThanOrEqual(n) {
    this.#check(this.actual <= n, `期望 ≤ ${n}，实际 ${this.actual}`)
  }

  toBeGreaterThan(n) {
    this.#check(this.actual > n, `期望 > ${n}，实际 ${this.actual}`)
  }
}

export function expect(actual) {
  return new Assertion(actual)
}

/** 由 runner 调用：打印结果并给出退出码。 */
export function localSummary() {
  console.log(`\n本地断言运行结果：通过 ${passed}，失败 ${failed}`)
  for (const f of failures) console.log(`  ✗ ${f.name}\n      ${f.message}`)
  return failed === 0
}
