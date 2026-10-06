// 模板静态检查：两条会让整页白屏的写法。
//
// 这两条都不是语法错误，`vite build` **编译期一律通过**，只有真正渲染时才出错；
// 而 Vue 一旦在渲染期抛异常就会卸载整棵组件树 —— 用户看到的是整页空白，
// 控制台之外没有任何提示。账单任务页先后因此白屏过两次，所以把它固化成检查。
//
//   1. 同一个元素上同时写 v-if 与 v-for。Vue 3 里 v-if 优先级更高，
//      求值时循环变量还不存在，访问它的属性直接抛 TypeError。
//   2. 循环变量被用在 v-for 的作用域之外（典型：给循环补一行兄弟节点，
//      却把它写在了带 v-for 的那个元素外面）。兄弟节点拿不到循环变量。
//
// 正确写法：过滤条件放进 computed；需要多节点循环时，把 v-for 写在
// <template v-for="..." :key="..."> 上，让所有兄弟节点都在作用域内。
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('../src', import.meta.url))

function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) out.push(...walk(p))
    else if (name.endsWith('.vue')) out.push(p)
  }
  return out
}

// 空元素：没有闭标签，也不可能包住别的节点。
const VOID_TAGS = new Set([
  'area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input',
  'link', 'meta', 'param', 'source', 'track', 'wbr',
])

// 把模板切成标签流。开标签、闭标签、自闭合都收进来——作用域要用它们配对。
const TOKEN_RE = /<(\/?)([a-zA-Z][\w-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)(\/?)>/g

function tokenize(tpl) {
  const tokens = []
  let m
  TOKEN_RE.lastIndex = 0
  while ((m = TOKEN_RE.exec(tpl))) {
    tokens.push({
      isClose: m[1] === '/',
      tag: m[2],
      attrs: m[3],
      selfClosing: m[4] === '/',
      start: m.index,
      end: m.index + m[0].length,
    })
  }
  return tokens
}

function lineAt(tpl, index) {
  return (tpl.slice(0, index).match(/\n/g) || []).length + 1
}

// 求某个开标签的配对闭标签之后的位置。
//
// 必须按**深度**配对，不能简单地找「下一个同名闭标签」：模板里嵌套的同名标签
// 非常常见（div 套 div 套 div），取第一个会把作用域截断在里层，
// 于是后面那些本来合法的引用全被误报成越界——一个只会狼来了的检查等于没有检查。
function scopeEnd(tokens, openIndex, tpl) {
  const open = tokens[openIndex]
  if (open.selfClosing || VOID_TAGS.has(open.tag)) return open.end

  let depth = 1
  for (let i = openIndex + 1; i < tokens.length; i++) {
    const t = tokens[i]
    if (t.tag !== open.tag) continue
    if (t.isClose) {
      depth--
      if (depth === 0) return t.end
    } else if (!t.selfClosing && !VOID_TAGS.has(t.tag)) {
      depth++
    }
  }
  // 没有配对闭标签（模板不完整）：保守地认为作用域一直到结尾，
  // 这样只会漏报而不会误报。
  return tpl.length
}

// 规则 1：同一元素上同时有 v-for 与 v-if。
function findConditionalLoop(tpl, tokens) {
  const out = []
  for (const t of tokens) {
    if (t.isClose) continue
    if (/\sv-for\s*=/.test(t.attrs) && /\sv-(?:if|else-if)\s*=/.test(t.attrs)) {
      out.push({ line: lineAt(tpl, t.start), tag: t.tag })
    }
  }
  return out
}

// 规则 2：循环变量被用在作用域之外。
//
// 关键点：**同一个别名会被多个循环复用**（模板里到处是 `v-for="r in ..."`），
// 所以不能逐个循环独立判断——那样第二个循环里的合法引用会被算成「逃出了第一个循环」。
// 正确判据是：某个引用只要落在**任意一个**声明了该别名的循环区间内，就算合法；
// 一个都不落的才是真越界。
function findAliasOutOfScope(tpl, tokens) {
  // 先按别名收集所有循环区间。
  const scopesByAlias = new Map()
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i]
    if (t.isClose) continue
    const forMatch = t.attrs.match(/\sv-for\s*=\s*"([^"]*)"/)
    if (!forMatch) continue
    // v-for="t in tasks" / v-for="(item, idx) in list" 都取第一个标识符。
    const aliasMatch = forMatch[1].match(/^\s*\(?\s*([A-Za-z_$][\w$]*)/)
    if (!aliasMatch) continue
    const alias = aliasMatch[1]
    if (!scopesByAlias.has(alias)) scopesByAlias.set(alias, [])
    scopesByAlias.get(alias).push({
      start: t.start,
      end: scopeEnd(tokens, i, tpl),
      loopLine: lineAt(tpl, t.start),
    })
  }

  const out = []
  for (const [alias, scopes] of scopesByAlias) {
    const aliasRe = new RegExp(`(^|[^\\w$.])${alias}\\s*[.\\[]`, 'g')
    let m
    aliasRe.lastIndex = 0
    while ((m = aliasRe.exec(tpl))) {
      const at = m.index + m[1].length
      if (scopes.some((s) => at >= s.start && at < s.end)) continue
      out.push({
        line: lineAt(tpl, at),
        alias,
        loopLine: scopes[0].loopLine,
        snippet: tpl.slice(at, at + 24).split('\n')[0].trim(),
      })
    }
  }
  // 同一位置不重复报（同名别名的多个循环会各扫一遍）。
  const seen = new Set()
  return out.filter((v) => {
    const k = `${v.line}:${v.snippet}`
    if (seen.has(k)) return false
    seen.add(k)
    return true
  })
}

let failed = 0
for (const file of walk(root)) {
  const src = readFileSync(file, 'utf8')
  const tplMatch = src.match(/<template>([\s\S]*)<\/template>/)
  if (!tplMatch) continue
  // 去掉注释：说明文字里会提到这些指令与变量，不该被算成违规。
  const tpl = tplMatch[1].replace(/<!--[\s\S]*?-->/g, '')
  const tokens = tokenize(tpl)
  const rel = file.slice(root.length + 1)

  for (const v of findConditionalLoop(tpl, tokens)) {
    console.error(
      `src/${rel}:${v.line} <${v.tag}> 上同时有 v-for 与 v-if —— ` +
        `v-if 先执行且拿不到循环变量，渲染期抛异常会让整页白屏`
    )
    failed++
  }

  for (const v of findAliasOutOfScope(tpl, tokens)) {
    console.error(
      `src/${rel}:${v.line} 用到了循环变量 ${v.alias}，但已出了它的作用域 —— ` +
        `它由第 ${v.loopLine} 行的 v-for 定义；这里求值为 undefined，` +
        `访问 ${v.snippet} 会在渲染期抛异常并让整页白屏。` +
        `若要多节点共用一个循环，请把 v-for 写到 <template v-for> 上`
    )
    failed++
  }
}

if (failed > 0) {
  console.error(`\n模板检查未通过：${failed} 处`)
  process.exit(1)
}
console.log('模板检查通过：v-if/v-for 同元素、循环变量越界 均未发现')
