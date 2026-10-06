// 模板静态检查：禁止把 v-if 与 v-for 写在同一个元素上。
//
// 为什么单拎这一条：Vue 3 里 v-if 的优先级**高于** v-for，所以 v-if 求值时
// 循环变量还不存在，访问它的属性会抛 TypeError。异常发生在渲染期，
// 而 Vue 一旦在渲染期抛异常就会卸载整棵组件树 —— 用户看到的是整页白屏，
// 除了控制台没有任何提示。更麻烦的是 `vite build` 只做编译，这种写法
// **编译期完全通过**，所以构建绿不代表页面能打开（账单任务页就这样白屏过一次）。
//
// 正确写法：把过滤条件放进 computed，模板里只用 v-for；确实要按项判断时，
// 用 <template v-for> 包一层，把 v-if 放到里面的元素上。
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

// 扫出「同一个标签上同时出现 v-for 与 v-if/v-else-if」的位置。
//
// 刻意用朴素的正则而不是 @vue/compiler-dom 的 AST：这是条红线检查，
// 多报几个（人工扫一眼就知道是不是误报）远好过因为解析器行为差异漏掉真问题。
function findViolations(tpl) {
  const problems = []
  const tagRe = /<([a-zA-Z][\w-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>/g
  let line = 1
  let consumed = 0
  let m
  while ((m = tagRe.exec(tpl))) {
    line += (tpl.slice(consumed, m.index).match(/\n/g) || []).length
    consumed = m.index
    const attrs = m[2]
    const hasFor = /\sv-for\s*=/.test(attrs)
    const hasIf = /\sv-(?:if|else-if)\s*=/.test(attrs)
    if (hasFor && hasIf) problems.push({ line, tag: m[1] })
    line += (m[0].match(/\n/g) || []).length
  }
  return problems
}

let failed = 0
for (const file of walk(root)) {
  const src = readFileSync(file, 'utf8')
  const tplMatch = src.match(/<template>([\s\S]*)<\/template>/)
  if (!tplMatch) continue
  // 去掉注释：说明文字里会提到这对指令，不该被算成违规。
  const tpl = tplMatch[1].replace(/<!--[\s\S]*?-->/g, '')
  for (const v of findViolations(tpl)) {
    const rel = file.slice(root.length + 1)
    console.error(
      `src/${rel}:${v.line} <${v.tag}> 上同时有 v-for 与 v-if —— ` +
        `v-if 先执行且拿不到循环变量，渲染期抛异常会让整页白屏`
    )
    failed++
  }
}

if (failed > 0) {
  console.error(`\n模板检查未通过：${failed} 处`)
  process.exit(1)
}
console.log('模板检查通过：没有 v-if 与 v-for 同元素')
