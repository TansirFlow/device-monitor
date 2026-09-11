// 看板截图工具（零依赖，只需 node + 本机 Chrome/Edge）。
//
//   node scripts/screenshot.mjs                       # 截 http://127.0.0.1:8080
//   node scripts/screenshot.mjs http://127.0.0.1:8080 docs/screenshots
//
// 会截三张图：
//   grid.png           卡片总览
//   drawer.png         点开卡片后的详情（含各指标历史曲线）
//   drawer-bottom.png  详情下半部分（温度传感器表、磁盘挂载点）
//
// 为什么需要它：render-smoke.mjs 只能验证"函数返回的 HTML 字符串对不对"，
// 验证不了版式。比如温度行的传感器名太长会把整行挤到换行、把一排卡片的
// 对齐全部带乱——这种问题只有真渲染一遍才看得见。

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';

const URL_ = process.argv[2] || 'http://127.0.0.1:8080/';
const OUT = path.resolve(process.argv[3] || 'docs/screenshots');
const WINDOW = '1440,1000';

// ---------- 找浏览器 ----------
function findBrowser() {
  const candidates = [
    process.env.CHROME_PATH,
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
    '/usr/bin/google-chrome',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
  ].filter(Boolean);
  for (const c of candidates) {
    try { if (fs.existsSync(c)) return c; } catch { /* ignore */ }
  }
  for (const name of ['google-chrome', 'chromium', 'chromium-browser', 'chrome']) {
    const p = which(name);
    if (p) return p;
  }
  return null;
}

function which(name) {
  const dirs = (process.env.PATH || '').split(path.delimiter);
  const exts = process.platform === 'win32' ? ['.exe', '.cmd', '.bat', ''] : [''];
  for (const d of dirs) {
    for (const e of exts) {
      const p = path.join(d, name + e);
      try { if (fs.existsSync(p)) return p; } catch { /* ignore */ }
    }
  }
  return null;
}

// ---------- CDP 最小客户端 ----------
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function makeClient(wsUrl) {
  let nextId = 1;
  const pending = new Map();
  const ws = new WebSocket(wsUrl);
  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data);
    const slot = msg.id && pending.get(msg.id);
    if (!slot) return;
    pending.delete(msg.id);
    if (msg.error) slot.reject(new Error(msg.method + ': ' + JSON.stringify(msg.error)));
    else slot.resolve(msg.result);
  });
  return {
    ready: new Promise((res, rej) => {
      ws.addEventListener('open', res);
      ws.addEventListener('error', () => rej(new Error('无法连接 CDP WebSocket')));
    }),
    send(method, params = {}) {
      return new Promise((resolve, reject) => {
        const id = nextId++;
        pending.set(id, { resolve, reject });
        ws.send(JSON.stringify({ id, method, params }));
      });
    },
    close: () => ws.close(),
  };
}

async function waitForCDP(port, timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (res.ok) return true;
    } catch { /* 还没起来 */ }
    await sleep(200);
  }
  throw new Error(`等不到 Chrome 的调试端口 ${port}（${timeoutMs}ms 超时）`);
}

async function firstPageTarget(port) {
  const res = await fetch(`http://127.0.0.1:${port}/json`);
  const list = await res.json();
  const t = list.find((x) => x.type === 'page');
  if (!t) throw new Error('没有找到可用的 page target');
  return t.webSocketDebuggerUrl;
}

// ---------- 主流程 ----------
const browser = findBrowser();
if (!browser) {
  console.error('找不到 Chrome/Edge。可以设置环境变量 CHROME_PATH 指定可执行文件。');
  process.exit(1);
}

fs.mkdirSync(OUT, { recursive: true });
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'mon-shot-'));
const port = 9333 + Math.floor(Math.random() * 200);

console.log(`浏览器: ${browser}`);
console.log(`目标  : ${URL_}`);
console.log(`输出  : ${OUT}`);

const child = spawn(browser, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  `--user-data-dir=${profile}`,
  '--hide-scrollbars', `--window-size=${WINDOW}`,
  `--remote-debugging-port=${port}`,
  'about:blank',
], { stdio: 'ignore' });

let client;
try {
  await waitForCDP(port);
  client = makeClient(await firstPageTarget(port));
  await client.ready;
  await client.send('Page.enable');
  await client.send('Runtime.enable');
  await client.send('Page.navigate', { url: URL_ });

  const shoot = async (file) => {
    const r = await client.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    const p = path.join(OUT, file);
    fs.writeFileSync(p, Buffer.from(r.data, 'base64'));
    console.log(`  → ${file}  ${(fs.statSync(p).size / 1024).toFixed(1)} KB`);
  };

  const evaluate = async (expr) => {
    const r = await client.send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error('页面内异常: ' + JSON.stringify(r.exceptionDetails));
    return r.result && r.result.value;
  };

  // 卡片总览。等久一点是因为首屏要等第一次 /api/v1/nodes 返回。
  await sleep(4500);
  const cards = await evaluate("document.querySelectorAll('.card').length");
  console.log(`卡片数量: ${cards}`);
  if (!cards) console.log('  提示：还没有节点上报，截出来会是空状态页。');
  await shoot('grid.png');

  // 详情抽屉：点开第一张卡片，再等历史曲线异步加载完
  if (cards > 0) {
    await evaluate("document.querySelector('.card').click()");
    await sleep(4500);
    const panels = await evaluate(
      "Array.from(document.querySelectorAll('#drawerBody .panel')).map(p=>p.querySelector('h4').textContent).join(' | ')",
    );
    console.log(`抽屉面板: ${panels}`);
    console.log(`已绘制图表: ${await evaluate("document.querySelectorAll('#drawerBody svg').length")}`);
    await shoot('drawer.png');
    await evaluate("document.querySelector('#drawerBody').scrollTop = 99999");
    await sleep(600);
    await shoot('drawer-bottom.png');
  }
} finally {
  if (client) client.close();
  child.kill();
  // 等进程真正退出再删 profile，否则 Windows 上会因文件占用失败
  await sleep(800);
  try { fs.rmSync(profile, { recursive: true, force: true }); } catch { /* ignore */ }
}
