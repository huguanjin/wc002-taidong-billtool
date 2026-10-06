// 剪贴板写入，供「生成账单」与「账单任务」两处复用。
//
// 为什么不能只用 navigator.clipboard：它只在**安全上下文**（https 或 localhost）存在。
// 这套工具是 http 部署的，那里 navigator.clipboard 直接是 undefined，
// 所以真正干活的是 execCommand('copy') 那条路径——它没有安全上下文要求，
// 只需要在一次用户手势里同步调用。把它写成「降级兜底」会让人误以为很少走到，
// 实际上线上每次复制走的都是它。

// copyText 把文本写进剪贴板。
//
// 返回 'ok' 表示已经复制好，可以直接给用户看成功提示；
// 返回 'fail' 表示两条路都失败，调用方应回退到「选中文本让用户自己按 Ctrl+C」。
export async function copyText(text) {
  if (!text) return 'fail'

  // 安全上下文下优先用异步剪贴板 API：它能真正写入系统剪贴板并返回结果。
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return 'ok'
    } catch (err) {
      // 例如页面没有聚焦、或用户拒绝了权限。继续往下试 execCommand。
    }
  }

  return legacyCopy(text) ? 'ok' : 'fail'
}

// legacyCopy 用临时 textarea + execCommand('copy') 写入。
//
// 必须放进 DOM 并 select()：脱离文档的节点选不中，execCommand 也就无内容可复制。
// textarea 而不是 input：多行文案在 input 里会被压成一行。
// 用 fixed + 透明定位而不是 display:none 或 visibility:hidden——隐藏元素无法被选中。
function legacyCopy(text) {
  if (!document.execCommand) return false

  const el = document.createElement('textarea')
  el.value = text
  // 只读可以避免移动端弹键盘，但它仍可被 select()。
  el.setAttribute('readonly', '')
  el.style.position = 'fixed'
  el.style.top = '0'
  el.style.left = '0'
  el.style.width = '1px'
  el.style.height = '1px'
  el.style.padding = '0'
  el.style.border = 'none'
  el.style.opacity = '0'
  document.body.appendChild(el)

  // 记住调用前的选中范围：临时 textarea 的选中会覆盖用户自己在页面上的选区，
  // 复制完不还原的话，用户会发现自己刚才选的东西没了。
  const selection = window.getSelection && window.getSelection()
  const savedRanges = []
  if (selection && selection.rangeCount > 0) {
    for (let i = 0; i < selection.rangeCount; i++) savedRanges.push(selection.getRangeAt(i))
  }

  let ok = false
  try {
    el.select()
    el.setSelectionRange(0, el.value.length)
    ok = document.execCommand('copy')
  } catch (err) {
    ok = false
  } finally {
    document.body.removeChild(el)
    if (selection) {
      selection.removeAllRanges()
      savedRanges.forEach((r) => selection.addRange(r))
    }
  }
  return ok
}

// selectElementText 选中一个元素的全部文本。
//
// copyText 失败时的最后兜底：文字高亮着，用户自己按 Ctrl+C 也能拿走。
export function selectElementText(el) {
  if (!el || !window.getSelection || !document.createRange) return
  const range = document.createRange()
  range.selectNodeContents(el)
  const sel = window.getSelection()
  sel.removeAllRanges()
  sel.addRange(range)
}
