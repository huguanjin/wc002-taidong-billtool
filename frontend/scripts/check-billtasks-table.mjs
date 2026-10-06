// 结构检查：某个 <tbody> 里的 v-for 是否把**所有**循环行都包住了。
//
// 为什么单查这一处：计划列表一个任务要渲染两行（数据行 + 可展开的摘要行）。
// 若 v-for 写在数据行那个 <tr> 上，摘要行就是它的**兄弟节点**，拿不到循环变量，
// 访问 t.id 会在渲染期抛异常、整页白屏——而 `vite build` 编译期完全通过。
//
// 这里用 Vue 自己的 AST 判断，而不是正则：要确认的正是「谁是兄弟、谁是后代」，
// 这只有语法树说得准。
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { parse as parseSFC } from '@vue/compiler-sfc'
import { baseParse } from '@vue/compiler-dom'

const file = fileURLToPath(new URL('../src/BillTasks.vue', import.meta.url))
const src = readFileSync(file, 'utf8')
const { descriptor, errors } = parseSFC(src)
if (errors.length) {
  console.error('SFC 解析失败：', errors)
  process.exit(1)
}

const ast = baseParse(descriptor.template.content)
const isFor = (n) => Array.isArray(n.props) && n.props.some((p) => p.type === 7 && p.name === 'for')

// 收集所有 tbody，以及其中每个直接子节点的形态。
function collectTbody(node, out = []) {
  if (node.type === 1) {
    if (node.tag === 'tbody') out.push(node)
  }
  for (const c of node.children || []) {
    if (c && typeof c === 'object') collectTbody(c, out)
  }
  return out
}

let failed = 0
for (const tbody of collectTbody(ast)) {
  const kids = (tbody.children || []).filter((c) => c.type === 1)
  const forTpl = kids.find((k) => k.tag === 'template' && isFor(k))
  const bareForTr = kids.find((k) => k.tag === 'tr' && isFor(k))

  // 情形 A：v-for 写在 <template> 上 → 里面的所有 tr 都在作用域内，正确。
  // 情形 B：v-for 写在某个 <tr> 上 → 检查它后面还有没有**同级**的 tr；
  //        有的话那些行拿不到循环变量，就是会导致白屏的写法。
  if (bareForTr) {
    const idx = kids.indexOf(bareForTr)
    const siblingsAfter = kids.slice(idx + 1).filter((k) => k.tag === 'tr')
    // 同级 tr 里若引用了循环变量，才是真问题（纯静态行不引用则无所谓）。
    for (const sib of siblingsAfter) {
      const text = JSON.stringify(sib)
      const alias = (bareForTr.props.find((p) => p.type === 7 && p.name === 'for')?.exp?.content || '').split(/\s+in\s+/)[0].trim()
      if (alias && new RegExp(`[^\\w$.]${alias}\\s*[.\\[]`).test(text)) {
        console.error(
          `BillTasks.vue 的计划列表 <tbody>：v-for 写在了 <tr> 上，` +
            `但它后面的兄弟 <tr> 引用了循环变量 ${alias} —— ` +
            `兄弟节点拿不到它，渲染期会抛异常并让整页白屏。` +
            `请把 v-for 移到 <template v-for="..."> 上，包住所有循环行`
        )
        failed++
      }
    }
  }

  if (!forTpl && !bareForTr) continue
  // 有 <template v-for> 时顺带确认它确实包住了多于一个 tr（即确实是多行循环）。
  if (forTpl) {
    const innerTrs = (forTpl.children || []).filter((c) => c.type === 1 && c.tag === 'tr')
    if (innerTrs.length === 0) {
      console.error('BillTasks.vue：<template v-for> 里没有任何 <tr>，可能包错了层级')
      failed++
    }
  }
}

if (failed > 0) {
  console.error(`\n结构与法检查未通过：${failed} 处`)
  process.exit(1)
}
console.log('结构检查通过：多行循环都写在 <template v-for> 上')
